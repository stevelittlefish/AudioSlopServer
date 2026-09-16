# CLAUDE.md — ASS (Audio Slop Server)

Guidance for working on this project.

> **Start here each session:** `TODO.md` tracks what's done and what's next.
> Read it before starting work, and keep it current — across sessions it's the
> closest thing to memory this project has.

## What it is

ASS is a single-GPU orchestration server for multiple heavyweight audio AI
services. It loads and evicts models on demand — like Ollama does for LLMs —
so that many disparate services can share **one** GPU instead of each
monopolizing its own.

## Core concept

- **One GPU, many models.** Only one (or a small set that fits) is resident in
  VRAM at a time.
- **Swap models in and out on demand**, driven by incoming requests.
- Target services include: DEMUCS (separation), Whisper (transcription),
  Stable Audio 3, YuE 2, ACE-Step 1.5 XL (generation), and more over time.

## Working agreement

- **Spec phase — no coding** until the user explicitly says to start building.
- Nuggets from the user get transcribed into README.md (product-facing) or
  this file (technical/architecture), whichever fits.
- **Commit straight to `main`.** For every dev task, commit directly to `main`
  unless told otherwise. No branches, no PRs — those are for boring corporate
  jobs and people who give a shit about the code. We're generating Slop; the
  project itself is Slop. Slop generating Slop.
- When committing, proudly announce: **"Slopping it straight to main!"**
- **Always `git push` after committing.** The first rule of Slop: Slop is for
  the masses, and the masses can't consume it while it's on our hard drive.
  Commit to `main`, then push it straight out.

## The Great Philosophy of Software Languages

1. **No JavaScript on the server. Ever.** We are not failed front-end
   engineers — we are failed *back-end* engineers. That's why we make the Slop!
   Avoid Node at all costs in committed server code.
   - **Exception:** JavaScript *is* meant for the front-end, so it's fine on the
     client. It's also acceptable to use Node/JS tooling to validate or build
     front-end code — just keep it out of the committed server code.

2. **Python is tolerated, not embraced.** Many of these AI tools are written in
   Python. This is unfortunate — it drags in the whole miserable circus of
   virtualenvs, requirements.txt, pyenv, poetry, conda, uv, and a thousand other
   stupid tools that exist purely to avoid installing packages in the system
   pip. We may have to commit *some* Python to the codebase, and that's
   accepted: Python is at least better than JavaScript. But if we can avoid it,
   we should.

3. **Go is our language of choice.** If it were up to us, the whole thing would
   be written in Go. Go is great because you just type `go run .` and it just
   works. We don't know how. We don't care. We just get a cool binary we can
   run. Go would be the language for everything here — if only the AI bastards
   hadn't all settled on Python as The One True Language™.
   - **Practical consequence:** default to Go for anything we control (the
     server, orchestration, glue, tooling). Fall back to Python only where the
     AI tooling forces our hand (see rule 2).

4. **No fancy-pants front-end frameworks.** React and all the other frameworks
   are hated. *If* we ever have a web interface (actual pages for humans):
   - Plain JavaScript only, where necessary.
   - Separate URLs per page, with server-generated HTML where possible.
   - No SPA, no build-step framework nonsense.

   That said, ASS will probably be **100% API** with no human-facing pages at
   all, so this rule may never come up. It's stated for the record because the
   frameworks are hated.

5. **No Object-Oriented Programming.** OOP is ideological nonsense invented by
   failed programmers with too much time on their hands. Write plain functions
   over plain data. No sprawling class hierarchies, no inheritance towers, no
   design-pattern cosplay. (Go makes this easy — lean on functions, structs, and
   composition, not ceremony.)

## Objectives & Coding Conventions

1. **ASCII art on startup.** When the app starts, it must print **ASS** in big
   ASCII-art letters to the log.
2. **Sarcasm required.** Both commit messages and code comments should include
   some sarcasm and witty remarks. Dry humor over dry documentation.
3. **Consistent API across sub-services.** Aim for a fairly consistent API
   shape across all the different audio sub-services, so callers don't have to
   relearn everything per service. Not a hard rule — if a service genuinely
   needs something different, exceptions are allowed.
4. **Config in TOML, not environment variables.** No environment variables
   except where absolutely necessary (i.e. something genuinely outside our
   control demands one). All configuration lives in TOML files. A dependency for
   TOML parsing is acceptable.
5. **Dependencies are expensive.** In general, treat every dependency as a cost
   to be justified. Prefer the standard library and a little of our own code
   over pulling something in. Some deps are worth it (TOML, sqlite) — but the
   default answer is "do we really need it?"
6. **Configurable memory footprint.** ASS must run on a 128GB server *and* on
   some poor peasant's 16GB laptop. Memory strategy (which services stay resident
   in RAM vs. get fully unloaded) is configured per service in TOML, with sane
   limits so we never assume the big-server case.
7. **Never vendor third-party source into this repo.** The boundary with every
   backend is HTTP, across a process/container line. We never copy their code in
   and never import it. This keeps ASS clear of copyleft (GPL etc.): running a
   GPL'd backend as a separate program we talk to over a socket is mere
   aggregation, not a derivative work, so ASS stays permissively licensed (MIT).
   Corollary: keep Go dependencies to permissive licenses (MIT/BSD/Apache) —
   those *are* linked into our binary. Need a backend's logic? Reimplement it or
   wrap it behind a service; don't paste it in.

## Architecture notes

The one-line version: **ASS is a Go orchestrator that owns a single GPU and
multiplexes a set of dockerised audio backends onto it, one (or a budgeted few)
at a time — Ollama, but the "models" are whole containers.**

### Why this shape

Each audio service (DEMUCS, ACE-Step, Stable Audio 3, YuE, Whisper, aligner) is
already a self-contained, dockerised HTTP server that loads its model onto CUDA
and serves a job API. That means:

- The Python dependency circus stays quarantined **inside each container**. ASS
  never imports it (see rule 7 — HTTP boundary, no vendoring).
- ASS's job is not model internals. It's a **bouncer**: only one backend gets
  the GPU at a time. Start it, wait for health, forward the request, and later
  free the card for the next one.

```
        SlopBC / clients
              │
        ┌─────▼─────┐
        │    ASS    │   Go: unified API + reverse proxy + arbiter + supervisor
        └─────┬─────┘   owns the one GPU + the sqlite job store
   ┌──────────┼───────────┬───────────┐
[DEMUCS]  [ACE-Step]  [StableA3]   [Whisper]…   docker containers
   only ONE resident in VRAM at a time; the rest parked or stopped
```

### Components (all our own Go)

- **API + reverse proxy** — one front door; normalizes requests and proxies the
  actual work to whichever backend is resident.
- **Arbiter** — the brain. Enforces "one model on the GPU at a time" (or a
  VRAM/RAM budget). Decides who to evict and to which state.
- **Supervisor** — starts/stops/park-signals backend containers via the Docker
  API; tracks each backend's state; owns the idle keep-warm timers.
- **Job store** — sqlite (`modernc.org/sqlite`), matching the house stack.

### Backend memory state machine

Cheapest-to-restore first. The arbiter's job is to get the target backend to
`pinned` and demote the current occupant to the cheapest state the RAM budget
and its config allow:

| State | Where the weights are | Restore cost |
|---|---|---|
| `pinned` | in VRAM, ready | 0s — the hot model |
| `parked` | container alive, weights in **CPU RAM** | ~2–5s PCIe copy back to GPU |
| `sleeping` | container alive, weights unloaded | deserialize from RAM/page-cache, no disk |
| `stopped` | container down, weights on disk | full cold start, 10–60s (page cache helps) |

`park` is the sweet spot: **no container startup at all** on a swap — we just
tell the running process to move its model to CPU (freeing VRAM) and back. It is
opt-in per backend (needs the `/park`+`/unpark` endpoints below); a backend
without them falls back to `stop`.

### Eviction policy: lazy (ComfyUI), not timed (Ollama)

We own a **dedicated** audio GPU — nothing else wants the card — so proactively
cooling the hot model on a timer (Ollama's model) buys nothing and costs a
reload every time we idle past the timeout. So:

- **VRAM eviction is lazy.** Keep the resident model `pinned` until a *different*
  model actually needs the card, then evict the least-recently-used resident to
  make room. **Never on a clock.** LRU picks the victim.
- **`idle_ttl` is repurposed to reclaim RAM, not VRAM.** It governs only the
  deeper `parked → stopped` demotion — handing *system RAM* back to the OS. Off
  by default (the big server keeps things parked forever); set it on constrained
  boxes where a parked process holding RAM indefinitely is rude.

### VRAM: fragmentation and the park context-tax

Fragmentation is **mostly a non-issue for us**, thanks to process isolation:

- The classic "free VRAM but not contiguous" OOM comes from PyTorch's caching
  allocator fragmenting inside one *long-lived* process. `stop` eviction kills
  the whole process, returning everything to the driver — the next model starts
  with a pristine address space, so no backend can fragment another's VRAM.
- Within a `parked` process that only ever loads the same model at the same
  shapes, intra-process fragmentation stays low. If it ever bites, the fix is one
  setting baked into the backend image:
  `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True`.

The thing that *actually* costs VRAM is **not** fragmentation but the **CUDA
context tax on parked processes**: an alive process holds a few hundred MB of
VRAM (context + cuDNN/cuBLAS workspaces) even with its weights on the CPU. So
`park` costs both RAM *and* a slice of VRAM. The VRAM budget must count a
per-parked-backend context reserve, not just the pinned model's size. Irrelevant
on a big card; another reason the peasant config leans toward `stop`.

### The backend contract

Every backend ASS drives is expected to expose (the existing services already do
most of this — the pattern is stem-separator's):

- `GET  /health` — readiness probe.
- `GET  /v1/info` — model, device, capabilities.
- `POST /v1/<verb>` — submit a job, returns `{ job_id, state }`. (`verb` is
  service-specific: `separate`, `generate`, `transcribe`, `align`, …)
- `GET  /v1/jobs/{id}` — `{ state: queued|running|succeeded|failed, ... }`.
- `GET  /v1/jobs/{id}/<artifact>` — download result(s).
- `POST /park` / `POST /unpark` — **our addition** (we fork these services, so we
  can). `park`: `model.to('cpu'); torch.cuda.empty_cache()`. `unpark`: back to
  cuda. `empty_cache()` is mandatory or VRAM never actually frees.

### ASS unified API (mirrors the backend job envelope)

Async everywhere — generation takes minutes. One envelope across services so
callers don't relearn per backend:

- `POST /v1/{service}/jobs` → `{ job_id }` — submit (ASS ensures the service is
  resident first; the request may queue behind a model swap).
- `GET  /v1/jobs/{id}` → `{ service, state, ... }` — poll (SSE stream later).
- `GET  /v1/jobs/{id}/result[/<name>]` → artifact.
- `GET  /v1/backends` → each backend's state, queue depth, last-used, VRAM/RAM.

### Config (TOML — rule 4)

```toml
[memory]
ram_budget_mb = 96000     # 128GB server. Peasant sets ~8000; things fall to "stop".

[gpu]
device = 0                # the one card ASS is allowed to use
vram_budget_mb = 24000    # card size, minus headroom. Counts the pinned model
                          # PLUS the context tax of every parked backend.
context_tax_mb = 500      # VRAM a parked (alive) process holds even with weights
                          # on the CPU. Charged per parked backend against the budget.

[services.demucs]
image = "stem-separation:local"
port  = 5336
verb  = "separate"
evict = "park"            # park | stop — how ASS frees the GPU (VRAM eviction is
                          # lazy either way; this only picks the demotion target)
ram_reserve_mb = 1000     # cost of keeping this parked in system RAM, for budgeting
idle_ttl = "0"            # 0 = never reclaim parked RAM. Peasant sets e.g. "10m"
                          # to demote parked -> stopped and hand RAM back.

[services.yue]
image = "yue:local"
port  = 5340
verb  = "generate"
evict = "stop"            # too heavy to keep parked; just unload it
```

### Concurrency

- Requests for the **same** resident backend run against it directly (its own
  job queue handles serialization).
- Requests for a **different** backend queue behind the arbiter, which performs
  the swap, then releases them. One swap in flight at a time — the GPU is the
  lock.

### Build order

1. **Slice 1 — one backend, end to end.** Boot (ASCII art), read TOML, drive
   stem-separator through the full lifecycle: start → health → accept job →
   proxy → return result → idle-evict. No swapping yet.
2. **Slice 2 — second backend + the real swap.** Add ACE-Step or the aligner;
   the arbiter now has a genuine evict-one/load-the-other decision. This is where
   the whole idea is proven.
3. **The rest** — mostly more TOML entries and the occasional per-service quirk.
