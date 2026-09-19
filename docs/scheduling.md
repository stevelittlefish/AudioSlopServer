# Scheduling: what happens when requests collide

The in-depth story of how ASS decides who gets the GPU, who waits, and for how
long. `CLAUDE.md` has the one-paragraph version ("one swap at a time, the GPU is
the lock"). This document is the rest of it, straight from the code, because
"requests queue behind the arbiter" hides a lot of detail that matters the
moment two clients want two different models.

Code map, so you can check the claims:

| Thing | Where |
|---|---|
| Job lifecycle (submit → acquire → forward → poll → harvest) | `internal/engine/engine.go` (`Submit`, `process`) |
| The arbiter: leases, planning, swapping | `internal/arbiter/arbiter.go` (`Acquire`, `planLocked`, `swap`) |
| Operator actions (park/unpark/stop by hand) | `internal/arbiter/operator.go` |
| Budget numbers per service | `ass.toml` (`vram_pinned_mb`, `vram_parked_mb`, `gpu.vram_budget_mb`) |

## The vocabulary

- **Residency** — where a backend is right now: `pinned` (on the GPU),
  `parked` (container alive, weights in CPU RAM, holding only a CUDA context
  tax on the card), or `stopped` (container down). The arbiter keeps one
  residency per service in an in-memory map.
- **Lease** — a counter on each backend. Every job that is *using* a backend
  holds one lease from the moment it acquires the backend until its results are
  harvested. **A backend with a non-zero lease count can never be evicted.**
  This is the single most important rule in the file.
- **Swap** — the slow operation of demoting one or more victims (park or stop)
  and promoting a target (unpark or cold start). **Only one swap runs at a
  time**, project-wide. There is a single `swapping` flag; whoever sets it
  owns the GPU until the swap finishes.
- **Budget** — `gpu.vram_budget_mb`. The arbiter sums every pinned backend's
  `vram_pinned_mb` plus every parked backend's `vram_parked_mb` (the context
  tax) and refuses any plan that would push the sum over budget.
  `gpu.max_resident` is an optional extra cap on the pin *count*.

## A job, start to finish

1. `POST /v1/{service}/jobs` writes a `queued` row to sqlite and returns the
   job id **immediately**. The HTTP request never waits for the GPU.
2. A goroutine is spawned per job with a detached context and a **30 minute
   job timeout** (`jobTimeout` in the engine). Everything below happens inside
   that goroutine; the client only ever polls.
3. The goroutine calls `Acquire(service)`. This is where all queuing happens
   (next section). When it returns, the backend is pinned and the job holds a
   lease.
4. The job body is forwarded to the backend's `POST /v1/<verb>`, the job is
   marked `running`, and ASS polls the backend's `/v1/jobs/{id}`.
5. On success ASS reads the backend's VRAM telemetry, then **harvests every
   artifact** into its own results store.
6. Only now does the deferred `release()` run: lease count goes down by one and
   the arbiter broadcasts to anyone waiting. From this instant the backend is
   evictable (if that was its last lease).

If anything fails, the job is marked `failed` with a readable reason, and the
lease is released the same way.

## Inside `Acquire`: the decision loop

`Acquire` takes the arbiter lock and loops until one of these happens:

1. **Fast path.** The service is already `pinned`. Take a lease, stamp
   `lastUsed`, return. Cost: a mutex. This path does **not** check whether
   anyone else is waiting to evict this backend (see *Starvation* below).
2. **Someone else is mid-swap.** `swapping` is true. Wait on the condition
   variable and re-run the loop when woken. The world will look different.
3. **Plan.** Call `planLocked(service)`:
   - If the target fits in the budget alongside whatever is resident, the plan
     is "no victims, go".
   - Otherwise, walk the **pinned backends with zero leases**, cheapest first
     (**lowest `priority` first, ties broken least-recently-used**), subtracting
     each one's freed VRAM (its pinned cost minus what it retains parked, if its
     `evict` policy is `park`) until the target fits. Those become the victims,
     in that order. One eviction is often not enough under a budget, so this can
     name several.
   - If the walk runs out of evictable backends before the target fits, the
     plan is "wait". Every resident that could make room is busy.
4. **Wait, if the plan said wait.** Block on the condition variable. Wake-ups
   come from any `release()` and from the end of any swap.
5. **Swap.** Set `swapping = true`, **drop the lock**, and do the slow work:
   demote each victim (`POST /park`, or stop the container), then promote the
   target (`POST /unpark` if it was parked, else cold-start the container and
   wait for `/health`). Residencies are written back under the lock as each
   step lands, so `/v1/backends` always shows reality even mid-swap, including
   a human-facing `phase` note ("parking", "cold-starting").
6. Re-take the lock, clear `swapping`, broadcast, take the lease, return.

The lock is dropped during step 5 on purpose. Status reads, same-backend leases
on other pinned models, and operator actions are never blocked on Docker or
backend HTTP I/O.

A cancelled context (timeout or shutdown) wakes the loop and returns the
context error. The job is then failed with `acquiring <service>: ...`.

## Worked example: ACE-Step is busy, YuE arrives

Real numbers from `ass.toml` on the 3090:

| | `vram_pinned_mb` | `vram_parked_mb` |
|---|---|---|
| acestep | 16000 | 700 |
| yue | 10500 | 1100 |
| `gpu.vram_budget_mb` | 21500 | |

Timeline:

1. **t=0, ACE-Step job A arrives.** ACE-Step is stopped. Plan: nothing resident,
   16000 ≤ 21500, no victims. A sets `swapping`, cold-starts ACE-Step, pins it,
   takes a lease (leases = 1), and forwards its request. Backend is now
   generating.
2. **t=5s, YuE job B arrives.** YuE is stopped. Plan: 16000 (acestep pinned) +
   10500 (yue) = 26500 > 21500. Need a victim. Candidates: pinned backends with
   zero leases. ACE-Step has one lease. **No candidates.** Plan says wait. B
   blocks.
3. **t=10s, ACE-Step job C arrives.** ACE-Step is pinned. Fast path: leases = 2.
   C forwards straight to the backend, which queues it behind A on its own job
   queue. B stays blocked.
4. **t=90s, A finishes.** ASS harvests A's stems, then releases: leases = 1.
   Broadcast. B wakes, re-plans, still no zero-lease candidates, waits again.
5. **t=180s, C finishes and releases.** leases = 0. Broadcast. B wakes.
   Plan: ACE-Step is now a candidate. Evicting it with `evict = "park"` frees
   16000 − 700 = 15300, leaving 700 + 10500 = 11200 ≤ 21500. Victims = [acestep].
6. B sets `swapping`, POSTs `/park` to ACE-Step (weights to CPU RAM, ~2–5s),
   cold-starts YuE, waits for health, pins it, takes a lease, forwards its
   request.
7. **Any ACE-Step job D arriving now** finds ACE-Step parked. Plan: 700 (its own
   park tax, already counted) → promoting adds 16000 − 700; total 10500 + 16000
   = 26500 > 21500. Victim needed. YuE holds B's lease. D waits until B is
   harvested. Then D parks YuE and unparks ACE-Step (the fast path back, no
   container start).

From the client's side: B's job id came back at t=5s in state `queued`, stayed
`queued` for three minutes, went `running` around t=200s, and `succeeded`
later. Nothing was ever interrupted. Two things a caller should take from this:

- **"queued" can mean "waiting for another model to drain"**, not just "waiting
  in the backend's own queue". `GET /v1/backends` shows why: the target's
  residency, the current occupant's lease count, and any in-flight phase.
- **A job never pre-empts a running job.** The GPU changes hands only at a
  lease-count-zero boundary.

## Same-backend requests

Requests for the backend that is already pinned never queue inside ASS. Each
takes a lease and is forwarded at once; the backend's own job queue serializes
the actual inference. ASS's concurrency story is "one *model* at a time", not
"one *job* at a time".

## Multiple residents

The budget, not a count, decides what fits. On the 21500 MB config, demucs
(1200 pinned) and stableaudio (6500 pinned) sit on the card together with
room to spare, and the arbiter will not evict anything to load one beside the
other. It evicts the LRU zero-lease residents **only as far as needed**.

Parked backends still cost their context tax. A card with three parked
backends has that many hundred MB fewer to give to the next pinned model, which
is why `vram_parked_mb` is measured per service, not guessed.

## Eviction priority: who dies first

By default the arbiter picks its victim by **LRU** — the least-recently-used
zero-lease resident is evicted to make room. That's fine until you have a
backend you'd rather keep hot (a Whisper that everything else depends on) and a
backend you're happy to throw off the card the moment anyone else wants it (a
YuE generation nobody's waiting on). LRU can't tell them apart; `priority` can.

Each `[services.*]` block takes one optional integer:

```toml
[services.whisper]
priority = 100     # higher = more valuable = evicted LAST

[services.yue]
priority = 10      # low = cheap/rare = evicted FIRST
```

The rule is one line: **evict the lowest-priority zero-lease resident; break
ties by least-recently-used.** So priority is the primary sort key and recency
is the tiebreak. Two consequences fall out of that single number:

- **Leave everything at the default (`0`) and you get pure LRU** — the old
  behaviour, unchanged. Priority only does something once two services differ.
- **Bump one service up and it becomes sticky** — it survives swaps until
  nothing lower-priority is resident to evict instead. Only when the high-value
  backend is the *only* zero-lease resident left does it get evicted.

What priority is **not**:

- **Not a pin.** A high priority still loses the card if it's the only thing
  that can free enough VRAM. If you need "never auto-evict while a request could
  use it", that's a starvation lever (see *Known gaps*), not this knob.
- **Not restore cost.** Whether a victim is cheap or expensive to bring back is
  already expressed by `evict = park|stop` and the residency state machine.
  Priority is about *value to keep hot*, deliberately kept separate so the
  victim choice stays a thing you can read straight off the config: look at the
  priorities and the last-used column and you know exactly who dies next.

Priority never overrides a lease. **A leased backend is never a candidate**,
whatever its priority — the lease rule wins, always. Priority only orders the
backends that were already evictable.

## Operator actions

`POST /v1/backends/{service}/park`, `/unpark`, `/stop` and
`POST /v1/backends/unload-all` go
through the same arbiter (`operator.go`), obeying the same two rules:

- They claim the single swap slot, so a button press cannot race an automatic
  swap.
- They are **refused** with a 409 and a `LeaseHeldError` if the backend has
  in-flight jobs. An operator cannot yank a model out from under a job. Come
  back when it drains.

### Preload all

`POST /v1/backends/preload-all` (the "Preload all" button) is the inverse of
unload-all: get every parkable backend into system RAM ahead of time so the
first request of the day pays an unpark, not a cold start. For each stopped
backend with `evict = "park"`, largest `vram_pinned_mb` first, it cold-starts
the backend onto the card and immediately parks it. Biggest first because the
transient room only shrinks as park taxes accumulate.

Rules:

- It **never stops anything.** If the card is too full to cold-start the next
  candidate it may park idle pinned residents to make room, since parked is
  where they would end up anyway. A leased resident, or one with
  `evict = "stop"`, is never touched, and the candidate is skipped with a
  reason instead.
- It checks `memory.ram_budget_mb` against the sum of every alive backend's
  `ram_reserve_mb`. This is currently the **only** place the RAM budget is
  enforced. The job path does not check it, because lazy eviction parks one
  thing at a time. Preload is what stacks parked models up, so it is where the
  peasant box needs protecting.
- Each candidate is its own swap-slot hold, so real jobs can run between
  steps. If a job grabs a freshly warmed backend before the park, the park is
  refused and that backend is reported skipped. Harmless.
- The call is slow: N cold starts back to back. The response body lists
  `preloaded` and `skipped` with reasons. The cards show each backend's phase
  meanwhile.

## Known gaps (as of 2026-09-19)

These are real behaviours of the current code, not hypotheticals. Recorded
here so nobody rediscovers them the hard way.

### Starvation: a busy backend can hold the card forever

The fast path takes a lease on a pinned backend without checking whether a
different service is waiting to evict it. If ACE-Step jobs keep arriving so the
lease count never reaches zero, a waiting YuE job never gets a plan. It sits
in `queued` until its **30 minute job timeout** fires, then fails with
`acquiring yue: context deadline exceeded`. The client sees a failed job with
no indication that it was simply out-prioritised.

On a single-user box this is unlikely. On a shared server driven by SlopBC
with several people generating at once it will happen eventually.

**Likely fix:** a per-backend `draining` flag. When a waiter has been refused a
plan because of this backend's leases, mark it draining; the fast path then
refuses new leases on a draining backend and those callers wait too. Once the
count hits zero the waiter swaps, clears the flag, and the deferred same-backend
callers re-plan (and will now queue for a swap *back*). This trades throughput
on the hot model for fairness. Not implemented; decide whether we want it before
the server has more than one user.

### No FIFO among waiters

Every release and every swap end does a broadcast. All waiters wake, all
re-plan, and the first to find a viable plan grabs the swap slot. Two jobs
waiting for two different services are not served in arrival order. In
practice one swaps, the other reassesses and usually swaps next, but there is
no guarantee, and with the starvation gap above the "usually" can become
"never".

A FIFO ticket per waiter would fix both ordering and starvation together, at
the cost of throughput on the currently pinned model. Same decision as above.

### A failed swap leaves victims demoted

Victims are demoted before the target is promoted. If the target's cold start
fails (bad image, backend never becomes healthy), the victims stay parked or
stopped; ASS does not roll them back. The next request for a victim simply
re-promotes it. Harmless, but it means a broken backend config can cause a
spurious park/unpark cycle on a working one. The `phase` note is cleared on
failure so the status view does not show "cold-starting" forever.

### The job timeout covers the wait

The 30 minute `jobTimeout` starts at submit and covers acquire, inference, and
harvest together. A job that waits 25 minutes for the card then needs 10
minutes of generation will fail mid-generation. If long waits become normal
the timeout should start at acquire, or be split into a queue timeout and a run
timeout.
