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
- [ ] Supervisor: start/stop a backend container via the Docker API
- [ ] Health-wait: poll backend `/health` until ready (with a timeout)
- [ ] Unified async job API: `POST /v1/{service}/jobs`, `GET /v1/jobs/{id}`,
      `GET /v1/jobs/{id}/result`, `GET /v1/backends`
- [ ] sqlite job store (`modernc.org/sqlite`)
- [ ] Reverse-proxy a real `/v1/separate` job through to stem-separator
- [ ] Lazy lifecycle: bring backend up on first job, leave it pinned
- [ ] End-to-end smoke test: submit audio, poll, download separated stems

## Next — Slice 2: second backend + the real swap

- [ ] Add a second backend (ACE-Step or forced-aligner)
- [ ] Arbiter: enforce one-model-on-GPU, LRU eviction, one swap in flight
- [ ] Implement `/park` + `/unpark` on a forked backend, wire up `evict = "park"`
- [ ] VRAM/RAM budgeting: count pinned model + per-parked context tax
- [ ] Prove a real evict-one / load-the-other swap under load

## Later — The rest

- [ ] Onboard remaining backends (Stable Audio 3, YuE, Whisper, aligner)
- [ ] `idle_ttl` parked→stopped RAM reclaim (the peasant path)
- [ ] SSE job streaming instead of poll-only
- [ ] Auth (backends already support an API key; decide if ASS fronts it)
- [ ] Real weight-size measurements → concrete budget defaults
