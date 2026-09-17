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

## Now — First real backend: stem-separator (Demucs)

Onboarding a genuine GPU service, replacing the mock for `demucs`. The heavy
torch path can only be tested on the GPU box (`ai.lemon.com`); everything short
of that is done.

- [x] Assess the gap: stem-separator predated ASS (old Stable Audio 3 shape). It
      already had /health, /v1/info, /v1/separate, /v1/jobs/{id}, async
      submit→poll→download. Missing: the `artifacts[]` shape, the `/result/{name}`
      download path, and /park + /unpark.
- [x] Conform the service (committed+pushed to stevelittlefish/stem-separator,
      no back-compat aliases — the only clients are moving to ASS anyway):
      `stems[]`→`artifacts[]` with {name, kind, content_type, bytes};
      `/v1/jobs/{id}/stem/{name}`→`/result/{name}`; POST /park + /unpark that
      move the Demucs weights CPU↔GPU with a mandatory empty_cache(), serialized
      against a running separation by a GPU lock; `parked` in /health + /v1/info.
- [x] ASS side needed zero code changes — it already expects `artifacts[]` and
      `/result/{name}` (the mock always returned that shape). Fixed `ass.toml`
      for real deployment though: `gpu.enabled = true` (was defaulting false!),
      pinned `SEP_PORT`/`SEP_MODEL` via per-service env (the backend reads
      SEP_PORT, not the generic PORT ASS injects), and mounted the weight cache.
- [x] **Real end-to-end on the GPU box** — DONE, verified on ai.lemon.com
      (2026-09-17). GHCR image pulled, `docker compose up` ran ASS on :2645;
      submitted a real clip → cold-start → separate → harvest → `succeeded` with
      the correct `artifacts[]` (vocals + no_vocals, audio/wav), real stereo WAVs
      served from ASS's own store, demucs `pinned` with the lease released.
      `/park` + `/unpark` both returned clean 200s on real CUDA — the `.model`
      assumption in `separation.py:park_model` holds, no fix needed. Procedure in
      [docs/deploy.md](docs/deploy.md).
- [x] **Measure the parked VRAM (context tax)** — DONE (ai.lemon.com, RTX 3090).
      Per-process nvidia-smi across unpark→park: demucs drops 1018 → 354 MiB, so
      it frees ~664 MiB of model weights and holds ~354 MiB context tax parked.
      `ass.toml` now sets `context_tax_mb = 400` (measured + headroom); full
      numbers in [docs/measurements.md](docs/measurements.md).

## Now — Second real backend: Stable Audio 3 (generation)

Onboarding SA3 the same way stem-separator was conformed. The heavy torch path
can only be tested on the GPU box (`ai.lemon.com`); everything short of that is
done.

- [x] Assess the gap: `stable-audio-3-docker` was in the old pre-conform shape
      (same as stem-separator started) — it had /health, /v1/model, /v1/generate,
      async submit→poll, but returned `outputs[]` with `audio_url`/`spectrogram_url`
      and downloaded via `/v1/jobs/{id}/audio?index=N` + `/spectrogram`. Missing:
      the `artifacts[]` shape, `/result/{name}`, `/park` + `/unpark`, `/v1/info`.
- [x] Conform the service (committed+pushed to stevelittlefish/stable-audio-3-docker,
      no back-compat aliases): `outputs[]`→`artifacts[]` with {name, kind,
      content_type, bytes} (one audio clip per batch element + its spectrogram PNG,
      named by filename); `/audio`+`/spectrogram`→`/result/{name}`;
      `/v1/model`→`/v1/info` (+ `parked`); `parked` in /health; POST /park +
      /unpark that move the whole model (DiT + pretransform + conditioner) CPU↔GPU
      with a mandatory empty_cache(), serialized against a running generation by a
      GPU lock. `TORCH_HOME=/cache/torch` added for the shared-cache convention.
- [x] Add the release CI: `.github/workflows/release.yml` + `make_release.sh`
      (copied from stem-separator) publishing `ghcr.io/stevelittlefish/stable-audio-3-docker`
      on a `v*` tag, same tag scheme (vX.Y.Z / vX.Y / vX / latest). Frees runner
      disk first (the CUDA+torch+flash-attn image is big); no HF token needed at
      build time (weights fetched at runtime into the shared cache).
- [x] ASS side: added `[services.stableaudio]` to `ass.toml` (port 5335, verb
      `generate`, evict `park`, shm 8gb, shared /cache mount). Zero ASS code
      changes — it already expects `artifacts[]` + `/result/{name}`.
- [ ] **Cut the first release** on stable-audio-3-docker (`./make_release.sh
      v0.1.0 "..."`) so `ghcr.io/.../stable-audio-3-docker:latest` exists to pull.
- [ ] **Real end-to-end on the GPU box** — pull the image, run ASS, submit a
      generate job, verify cold-start → generate → harvest → `succeeded` with the
      right `artifacts[]`, and that `/park`+`/unpark` return clean 200s on real
      CUDA (confirm the `model.model.to(...)` park assumption holds, as we did for
      demucs's `.model`).
- [ ] **Measure** SA3's resident + parked VRAM and RAM footprint → replace the
      guessed `ram_reserve_mb = 8000` with real numbers (docs/measurements.md).

## Later — The rest

- [ ] Onboard remaining backends (YuE, Whisper, aligner)
- [ ] `idle_ttl` parked→stopped RAM reclaim (the peasant path)
- [ ] SSE job streaming instead of poll-only
- [ ] Auth (backends already support an API key; decide if ASS fronts it)
- [ ] Real weight-size measurements → concrete budget defaults. **Started** —
      see [docs/measurements.md](docs/measurements.md). First data (RTX 4080 /
      16GB box `ai2.lemon.com`): ~0.39GB VRAM baseline with everything off;
      demucs `htdemucs_ft` ~0.5GB resident, ~1.6GB peak. Demucs is small. Still
      need: measure the real parked context tax, and other models.
- [ ] **Smarter eviction than "evict on count."** The strong case: a small
      service (tiny VRAM footprint) shouldn't be evicted at all just because a
      different model wants the card — it can ride along. Once eviction is
      VRAM-budget-driven (not a pin count), the arbiter should keep a **fit-set**
      resident and only evict when the newcomer genuinely doesn't fit, preferring
      to evict big models over small ones. Consider a per-service "sticky"/pin
      flag so cheap always-useful backends (e.g. an aligner) stay resident
      indefinitely. Depends on the deferred VRAM budgeting + real weight sizes
      above.
