# TODO

A checklist, not a Jira board. No swimlanes, no story points, no one asking you
to "groom the backlog." Check things off, add things at the bottom, move on with
your life.

(Also the closest thing Claude has to long-term memory across sessions, so keep
it honest — if it's done, tick it; if it's abandoned, say so.)

## Done — Spec

- [x] Name the thing, state the problem, write the README pitch
- [x] The Great Philosophy of Software Languages (5 commandments)
- [x] Objectives & coding conventions (ASCII art, sarcasm, consistent API, TOML,
      cheap deps, configurable memory, no vendoring)
- [x] Version-control doctrine: Slop straight to main, then push
- [x] References setup: gitignored clones, LAN-gated `pull.sh`, 9 repos
- [x] MIT license + no-vendoring / HTTP-boundary rule
- [x] Architecture notes: bouncer-owns-GPU, park/stop state machine, backend
      contract, unified async API, lazy eviction, VRAM context-tax

## Now — Slice 1: one backend, end to end

Drive **stem-separator** through the whole lifecycle. No swapping yet.

- [x] Project skeleton: `go.mod`, `cmd/ass`, `internal/…`, `go run .` works
- [x] Boot sequence prints **ASS** in big ASCII art (non-negotiable, rule)
- [x] TOML config load (`[memory]`, `[gpu]`, `[services.*]`)
- [x] Supervisor: start/stop a backend container via the Docker API
- [x] Health-wait: poll backend `/health` until ready (with a timeout)
- [x] Unified async job API: `POST /v1/{service}/jobs`, `GET /v1/jobs/{id}`,
      `GET /v1/jobs/{id}/result`, `GET /v1/backends`
- [x] sqlite job store (`modernc.org/sqlite`) + artifacts table
- [x] On-disk results store + harvest-on-completion (artifacts outlive the backend)
- [x] Reverse-proxy a real job through to a backend (verified vs mock backend)
- [x] Lazy lifecycle: bring backend up on first job, leave it pinned
- [x] End-to-end smoke test: submit, poll, harvest, download multi-type artifacts

**Slice 1 done** — full vertical path works end to end against the mock backend,
no GPU. Results survive backend eviction (verified). Not yet done in slice 1:
persist backend job id across ASS restarts; stream large uploads to a temp file
instead of buffering. Both noted for later.

## Now — Slice 2: second backend + the real swap

- [x] Add a second backend — dev config already runs two mock backends
      (`demucs` evict=park, `yue` evict=stop), enough to prove the swap without
      standing up a real GPU service. A genuinely distinct one (ACE-Step/aligner)
      is a "Later" onboarding task, not a Slice 2 blocker.
- [x] Arbiter (`internal/arbiter`): enforces `max_resident` pins on the GPU
      (default 1 = one model on the card), LRU victim selection, one swap in
      flight (the GPU is the lock), and per-job **leases** so a running job can't
      be evicted before its results are harvested. Race-clean, unit-tested.
- [x] Wire `/park` + `/unpark`: `evict = "park"` demotes to CPU RAM (container
      stays alive) and re-promotes via the fast unpark path; `evict = "stop"`
      kills the container. Both exercised end to end.
- [x] Prove a real evict-one / load-the-other swap — verified against the two
      mock backends: demucs pinned → yue needed → demucs **parked** + yue pinned
      → demucs needed → yue **stopped** + demucs **unparked** (not cold-started).
      Container reality matched (`ass-yue` exited, `ass-demucs` running).

**Slice 2 done** — the whole idea is proven: one GPU slot, two models taking
turns, cheap park-swap and full stop-swap both working, jobs protected mid-flight
by leases. `/v1/backends` now reports live residency + lease counts.

Not yet done in slice 2 (deferred — needs data we don't have):
- [ ] VRAM/RAM budgeting: count pinned model + per-parked context tax against the
      budget. Needs real per-service weight sizes (see "Real weight-size
      measurements" below); today the invariant is a simple pin *count*, not MB.
- [ ] LRU across >1 resident: victim selection is real LRU code, but with
      `max_resident = 1` there's only ever one victim. Exercise it once budgeting
      allows a fit-set of 2+.

## Later — The rest

- [ ] Onboard remaining backends (Stable Audio 3, YuE, Whisper, aligner)
- [ ] `idle_ttl` parked→stopped RAM reclaim (the peasant path)
- [ ] SSE job streaming instead of poll-only
- [ ] Auth (backends already support an API key; decide if ASS fronts it)
- [ ] Real weight-size measurements → concrete budget defaults
