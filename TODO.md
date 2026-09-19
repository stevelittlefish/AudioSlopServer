# TODO

A checklist, not a Jira board. No swimlanes, no story points, no one asking you
to "groom the backlog." Check things off, add things at the bottom, move on with
your life.

(Also the closest thing Claude has to long-term memory across sessions, so keep
it honest — if it's done, tick it; if it's abandoned, say so.)

> ✅ **FIXNOW code done** — all four repos committed + pushed:
> [`FIXNOW-cache-and-dockerfiles.md`](FIXNOW-cache-and-dockerfiles.md) (per-service
> cache + one shared HF token, deps-before-source Dockerfiles, SA3 announces its
> weight download, ACE-Step defaults verified). **Remaining human steps:** cut the
> three fork releases (`make_release.sh v1.1.0 …`) and pull/smoke-test on the box —
> the full runbook is in [`DEPLOY.md`](DEPLOY.md).

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

- [x] **Clean slate on startup** (`Supervisor.CleanSlate`, called in `cmd/ass/main.go`):
      reap every `ass.service`-labeled container on boot so the arbiter's fresh,
      empty, in-memory residency map is actually true. Without it, backends left
      running from a previous ASS process are ghosts — invisible to the new arbiter,
      still holding VRAM, never evicted, one swap from an OOM. Also incidentally
      fixes stale images (a reaped container can't ignore a freshly pulled `:latest`).
      Chose blunt-reap over adopt-and-reconcile (YAGNI: restarts are rare, starts
      lazy, jobs async — a cold reload on restart is a fair price for no ghosts).
      `docker.List(label)` added as the one new primitive; tested against the mock.

Done in slice 2:
- [x] VRAM budgeting (MB, not a pin count). The arbiter sums each pinned backend's
      `vram_pinned_mb` + each parked backend's `vram_parked_mb` context tax and keeps
      the total under `gpu.vram_budget_mb`; `max_resident = 0` means "unlimited, let
      the budget decide." Per-service costs are configurable so an OOM is a one-line
      tweak (raise that service's `vram_pinned_mb`). `scripts/measure-vram.sh` produces
      the numbers. With no budget set it falls back to the old pin-count gate.
- [x] LRU across >1 resident + multi-victim eviction: one eviction often can't free
      enough (evicting demucs won't seat ACE-Step), so `planLocked` evicts LRU
      non-leased pinned backends one at a time until the newcomer fits. Covered by
      TestVRAMBudget.

- [x] VRAM self-reporting + recording. Every backend fork reports its GPU memory
      in `/v1/info` (`allocated_mb`/`reserved_mb`/`peak_mb`); ASS reads it after each
      job into the `vram_samples` table and serves the per-service rollup at
      `GET /v1/vram` next to the configured budget. peak_mb is torch's high-water
      mark, so one post-job read captures the inference peak — no GPU on ASS, no
      polling, no deps. The calibration loop for the estimates below.

Still to do here:
- [ ] Confirm the estimated `vram_pinned_mb` / `vram_parked_mb` on the box — now
      easiest via real traffic + `GET /v1/vram` (demucs measured; SA3, ACE-Step,
      YuE flagged UNMEASURED in ass.toml). Deploy the new backend images first so
      they emit the vram block.
- [ ] Hardening (later): the budget is SOFT — ASS trusts the declared numbers, it
      doesn't watch the card. A future step could reconcile against live nvidia-smi
      and refuse to pin when the card is actually fuller than declared.

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
- [x] **Cut the first release** on stable-audio-3-docker — done; GHCR has
      `v1.0.0` / `v1.0` / `v1` / `latest` (cold CI build ~20 min).
- [x] **Real end-to-end on the GPU box** — DONE, verified on ai.lemon.com
      (2026-09-17). Submitted a generate job to ASS → cold start (~9 min: gated
      weight download + flash-attn + model load) → generate (~5s) → harvest →
      `succeeded` with the right `artifacts[]` (`output_0.wav` audio/wav +
      `spectrogram_0.png` image/png), both served from ASS's own store. The
      artifacts[] conform is correct. `/park` + `/unpark` returned clean 200s on
      real CUDA — the `model.model.to('cpu')` + `empty_cache()` assumption holds,
      no fix needed — and a second generation after the park/unpark round-trip
      succeeded, so the model survives the CPU↔GPU bounce.
- [ ] **Measure** SA3's resident + parked VRAM (context tax) and RAM footprint →
      replace the guessed `ram_reserve_mb = 8000` with real numbers
      (docs/measurements.md). Needs per-process `nvidia-smi` on the box across an
      unpark→park (as done for demucs); can't be read over HTTP from a dev box.

## Now — Third real backend: ACE-Step 1.5 XL (generation)

Onboarding ACE-Step the same way stem-separator and SA3 were conformed. Fork is
`stevelittlefish/ACE-Step-1.5-inference-server` (renamed from `ACE-Step-1.5`;
cloned in `child_services/ACE-Step-1.5-inference-server`). Heavy torch path
tests only on the GPU box.

**Two things make this the biggest conform yet:**

1. **It's SlopBC's live workhorse.** SlopBC already talks to ACE-Step directly
   (`references/SlopBC/internal/engine`, via `/release_task` + `/query_result`)
   and has reverse-engineered the whole thing — see
   `references/SlopBC/docs/ace-step.md` (READ IT; it's gold: model zoo, the
   determinism recipe, `thinking`/LM gating, cover/repaint/analyze, audio_codes,
   the multipart source-audio path). Ripping the old API (user: "we don't need to
   keep it") means SlopBC must move to fronting ACE-Step **through ASS** — a
   coordination point, not a blocker, but decide the cutover before deleting
   routes.
2. **It's not a single-output text2music box like SA3.** It has multiple
   `task_type`s (text2music, cover, repaint, extract, analyze), a
   **multipart source-audio** path (`ctx_audio`/`ref_audio` for cover/repaint),
   and rich structured output beyond the WAV: `audio_codes` (the 5Hz blueprint),
   `metas`, `cot_caption`/`cot_lyrics`, `seed_value`. The ASS `generate` envelope
   covers text2music cleanly; the advanced flows need the artifact + params
   mapping thought through (below). Training is **out of scope** — inference only
   (user: "not sure it ever will be" a concern); we don't wrap `/v1/training/*`,
   `/v1/dataset/*`, LoRA-train, or the OpenRouter `/v1/chat/completions` adapter.

### The gap (assessed 2026-09-17, code-checked against the fork)

Current ACE-Step API vs. our backend contract. Every response is wrapped in
`wrap_response(...)` → `{data, code, ...}`; ASS expects **bare** shapes, so the
wrapper has to go (or be bypassed) on the conformed routes.

| Contract needs | ACE-Step has today | Gap |
|---|---|---|
| `POST /v1/generate` → `{job_id, state}` | `POST /release_task` → `{data:{task_id,status,queue_position}}` (JSON **or** multipart) | rename + unwrap; `task_id`→`job_id` |
| `GET /v1/jobs/{id}` → `{state, artifacts[]}` | `POST /query_result` w/ `task_id_list` → `[{task_id, result(JSON string), status(int)}]` | GET-by-id, unwrap, **build `artifacts[]`**, map int status→queued/running/succeeded/failed |
| `GET /v1/jobs/{id}/result/{name}` | `GET /v1/audio?path=<server-disk-path>` (allowed-dir gated) | serve by artifact **name**, not by leaking a disk path |
| `GET /v1/info` (model, device, caps) | `/v1/models` + `/v1/model_inventory` + `/v1/stats`; `/health` exists but custom shape | add `/v1/info`; add `parked` to `/health` |
| `POST /park` / `POST /unpark` | **absent** | add them (DiT + LM, if loaded, CPU↔GPU + `empty_cache()`, GPU-locked vs a running gen) |

**Artifacts mapping (the real design work).** A generation's `result` JSON string
carries `audio_paths[]` (relative `/v1/audio?path=` refs), `metas`, `seed_value`,
`audio_codes`, `cot_caption`/`cot_lyrics`. Proposed artifact set per job:

- one `kind=audio` per batch element (the WAV/mp3/flac — honour `audio_format`),
- `kind=metadata` for `audio_codes` (the reusable 5Hz blueprint — the fork
  already went to two patches to surface it; don't drop it here),
- `kind=lyrics` for `cot_lyrics` when present, `kind=metadata` for `metas` +
  `seed_value`.

This is exactly the "YuE returns a FLAC *plus* score *plus* lyrics" case the
artifact contract was designed for — ACE-Step is the first real backend to
exercise the mixed-kind artifact set, not just audio+spectrogram.

- [x] **Assess the gap** — DONE (this section), code-checked against the fork:
      `release_task_route.py`, `query_result_route.py`/`_service.py`,
      `audio_route.py`, `model_service_routes.py`, `job_result_payload.py`, plus
      SlopBC's `docs/ace-step.md`.
- [x] **Merge upstream into the fork** — DONE (2026-09-17). Merged
      `ace-step/ACE-Step-1.5` into the fork's `main` via a local merge (the
      GitHub cross-fork PR couldn't merge in-browser — conflicts + can't edit
      someone else's head branch). One conflict only: `inference.py` `dcw_enabled`.
      The fork's local patches (audio_codes surfacing, GPU config) auto-merged.
- [x] **Non-turbo garbles — RESOLVED by the merge, not a fork patch.** Upstream
      had already fixed the exact bug at the root (issue #1259): `dcw_enabled`
      defaults to `None` and resolves **per-model — DCW on for turbo, OFF for
      non-turbo** (`generate_music._resolve_dcw_enabled`), and upstream even
      exposed `dcw_enabled` as a REST field. That supersedes our old blanket
      `ACESTEP_DCW_ENABLED` env toggle (which defaulted ON for everyone and
      re-broke non-turbo), so the conflict was resolved by **taking upstream and
      dropping the fork patch**. Non-turbo (sft/base) should now work out of the
      box; verify by ear on the GPU box.
- [x] **SlopBC cutover — DECIDED: rip the legacy API out now** (user's call).
      No back-compat aliases. **Consequence: SlopFM breaks the moment this image
      deploys, until SlopBC is repointed at ASS's `/v1/{service}/jobs`.** That
      SlopBC move is now a required follow-up (see "Backends & orchestration").
- [x] **Conform the service** — DONE (fork, pushed). New ASS-contract HTTP layer:
      `POST /v1/generate` → bare `{job_id, state}`; `GET /v1/jobs/{id}` → bare
      `{state, artifacts[]}` (mixed-kind: audio + `audio_codes.txt` + `lyrics.txt`
      + `metadata.json`); `GET /v1/jobs/{id}/result/{name}` serves by name (no
      disk-path leak); `GET /v1/info`; `parked` added to `/health`. Legacy
      `/release_task` + `/query_result` + `/v1/audio` and their orphaned
      modules/tests **deleted**. Pure reshaping logic in `ass_artifacts.py` (stdlib,
      9 unit tests passing locally); FastAPI wiring in `ass_contract_routes.py`;
      `route_setup.py` + its wiring test updated. Multipart source-audio path
      (`ctx_audio`/`ref_audio` for cover/repaint) preserved via the reused parser.
      `py_compile` clean; heavy CUDA path unexercised (no GPU here).
- [x] **Add `/park` + `/unpark`** — DONE (fork). Reuse the handler's own
      `_recursive_to_device` (handles quantized/LoRA weights `.to()` misses) +
      `_empty_cache()` to move model/vae/text_encoder CPU↔GPU across every loaded
      handler; refuse with 409 while a job is running; serialize against model
      init via the shared `_init_lock`; track state in `app.state.parked`.
      **Correctness on real CUDA still needs the box** (same caveat SA3 had).
- [x] **Release CI — already provided by upstream.** The fork's
      `.github/workflows/container.yml` already builds + publishes
      `ghcr.io/…/ace-step-1.5-inference-server` to GHCR on a `v*` tag (+
      workflow_dispatch). No need to copy SA3's `release.yml`. Just **cut the
      first `v*` tag** when ready.
- [x] **ASS side** — DONE. Added `[services.acestep]` to `ass.toml` (port 8001,
      verb `generate`, evict `stop` to start, `env` pins `ACESTEP_API_PORT` +
      `ACESTEP_MODE=api`, shared `/cache` mount + a `/app/checkpoints` persistent
      mount + outputs). Zero ASS **code** changes, as expected. Also patched the
      fork Dockerfile to the shared-cache convention (`HF_HOME=/cache/huggingface`,
      `TORCH_HOME=/cache/torch`).
- [ ] **Real end-to-end on the GPU box** — the whole heavy path is unverified:
      cold-start → text2music generate → harvest → `succeeded` with the mixed-kind
      artifacts[]; then `/park`+`/unpark` on real CUDA; then non-turbo (sft) to
      confirm the DCW fix; then cover/repaint's multipart path. **Verify the
      `/app/checkpoints` mount layout** actually persists the model zoo (ACE-Step
      downloads there, not only into the HF cache — unconfirmed).
- [ ] **Measure** resident + parked VRAM + RAM → real `ram_reserve_mb` /
      `context_tax_mb` in docs/measurements.md (per-process `nvidia-smi` on box);
      then decide whether to flip `evict = "stop"` → `"park"`.

## Now — Slice 3: the web console

A small server-rendered web UI for the humans running ASS. Two jobs: an **admin
panel** to watch and drive backends by hand, and a **generic test page** per
service to fire real jobs from the browser (replacing the mismatched per-service
Gradio apps). Makes testing every current and future backend trivial.

**Philosophy guardrails (CLAUDE.md rules 1 & 4):** no SPA, no framework, no build
step. Go `html/template` renders server-side HTML; a little vanilla JS does the
polling and file upload. No JS on the *server*. One URL per page (`/admin`,
`/test/{service}`). No npm in committed server code.

Split so the useful server-side half lands even if the UI drags:

### Part A — operator control endpoints (useful with curl alone)

- [x] **On-demand park/unpark/stop endpoints.** Added operator-triggered actions
      (`internal/arbiter/operator.go` + wired in `internal/api/api.go`):
      `POST /v1/backends/{service}/park`, `/unpark`, `/stop`, and
      `POST /v1/backends/unload-all` (stops every idle resident, hands the GPU
      back). Each single-backend action returns the resulting residency; typed
      arbiter errors map to HTTP codes (unknown→404, park-unsupported→400,
      wrong-state/GPU-busy/lease-held→409). Idempotent no-ops where sensible.
- [x] **Route them THROUGH the arbiter, not around it.** All actions go via a
      shared `operate()` spine that claims the single GPU swap slot (so a button
      press can't race the arbiter's own swap) and respects **leases**: park/stop
      of a backend with in-flight jobs is refused with a 409 + how many jobs hold
      it (`LeaseHeldError`) — we chose refuse-with-409 over queuing (simplest,
      least surprising). unload-all skips busy backends and reports them rather
      than failing the whole call. Unit-tested + race-clean
      (`operator_test.go`): park/stop/unpark, lease protection, evict-to-make-room
      on unpark, unload-all skip/report, and the typed-error cases.
- [x] **Auth / bind story.** Decided: no one's using this yet, so keep it simple.
      The console rides the main API listener (`:2645`, already 0.0.0.0), no auth.
      Added a `[web]` table with `enabled` (a `*bool`, default **on** — absent
      table = enabled); `enabled = false` drops the operator routes entirely for
      a strictly API-only box. Documented the no-auth / keep-it-on-a-LAN caveat in
      `ass.toml` + README. Real auth stays deferred to "Later" until someone
      actually needs to face this at a network.

**Part A done** — operator controls (park/unpark/stop/unload-all) land on `:2645`,
lease-safe through the arbiter, gated by `[web] enabled` (default on). Useful with
curl today; the admin panel (Part B) just needs to render + POST to them.

### Part B — the admin panel

- [x] `GET /admin` — dark, self-contained console (`internal/web/admin.html`,
      embedded + served by `internal/web/web.go`). Cards per backend with a
      coloured residency badge (pinned green / parked amber / stopped grey),
      name, verb, image, lease count, evict policy, last-used. `GET /{$}`
      redirects to `/admin`. Gated behind `[web] enabled` alongside the operator
      endpoints. (VRAM/RAM columns wait on the deferred budgeting work.)
- [x] Buttons wired to the Part A endpoints: park / unpark / stop per backend
      (auto-disabled when N/A for the current state), plus the **unload-all**
      button. Vanilla JS POSTs, toasts the result, then refreshes.
- [x] Auto-refresh: polls `/v1/backends` every 3s (pauses during an action and
      while the tab's hidden; refreshes on focus). SSE is a later nicety.

**Part B done** — verified end to end against the two mock backends: dashboard
serves, `/` redirects, submit→pinned shows up, park/unpark/stop and unload-all
all work from the API the buttons call, and the evict=stop→park 400 surfaces as
a toast. First working design; **6 alternate designs next** for the user to pick.
(Couldn't self-screenshot — Chrome extension wasn't connected — but the user is
viewing it live.)

### Part C — the per-service test page

- [x] `GET /test/{service}` — a page driven by the shared job envelope (upload,
      submit `POST /v1/{service}/jobs`, poll `GET /v1/jobs/{id}`, then render
      `artifacts[]` with inline `<audio>` players for audio/* + download links).
      404s unknown services (`internal/web/web.go`); reads the backend's `verb`
      from `/v1/backends` to pick the form. `internal/web/test.html`, styled to
      match the admin panel. Verified end to end against the mock demucs backend
      (drop file → two-stem → succeeded → two audio/wav stems play + download).
- [x] Per-verb forms with a generic fallback: **separate** (mode/format/shifts/
      overlap + drag-drop audio) and **generate** (SA3) both fully built; unknown
      verbs get a raw-params JSON textarea + optional file, so the page is useful
      for any future backend. (Sourcing field lists/ranges from each backend's
      `/v1/info` is a later polish — the forms are hardcoded per verb for now.)
- [x] **SA3 generate form** — three workflows via a segmented toggle:
      text→audio, variation (init_audio + init_noise_level), inpaint
      (inpaint_audio + mask starts/ends), each posting to the right multipart
      file field. Advanced drawer: negative_prompt, cfg_scale, batch_size, LoRA
      strength, the 7 output formats, return_spectrogram. Params/field names taken
      from SlopBC's SA3 client + the conformed schema. Spectrogram/image
      artifacts render inline as `<img>`. Verified end to end against the mock
      generate backend (text path); variation/inpaint need real SA3 to exercise.
- [x] Cross-links: each admin card has a `test ▸` link to its test page; the test
      page has a `‹ admin` back link. (A dedicated index page is unnecessary — the
      admin panel already lists everything with links.)

- [x] **Refactored to a bespoke page per service** (the one-page-keyed-by-verb
      design was broken: acestep and stableaudio both use verb `generate`, so
      acestep would have rendered SA3's form and posted SA3 params at it). Now
      `internal/web/web.go` maps each service name → its own page
      (`test-demucs.html`, `test-stableaudio.html`, `test-acestep.html`), with
      the old generic raw-params form (`test.html`) as the fallback for any
      un-paged backend. Shared chrome (CSS + the poll/submit/render runtime)
      moved to `assets/test.css` + `assets/test-common.js`, both served from the
      embed FS, so each page carries only its form + a `build()`. `build()`
      returns an `encoding` (`json` | `blob` | `paramobj`) because ASS forwards
      the body verbatim and the three backends want different shapes. Covered by
      `web_test.go` (per-service serve, 404, fallback, shared assets, redirect).
- [x] **ACE-Step generate form** — four task types via a segmented toggle:
      text→music, cover, repaint, extract. Caption + lyrics + thinking (5Hz LM)
      + vocal language + duration/steps/guidance/seed always visible; cover
      (cover/noise strength), repaint (start/end/mode/strength) and extract
      (track_name + codes-only) reveal their own source-audio drop + controls.
      Advanced drawer: model, DCW (auto/on/off tri-state), bpm/key/time-sig,
      LM temp/cfg/top-p, CoT caption, constrained decoding. text→music posts a
      JSON body; source-audio tasks post multipart with the params as a
      `param_obj` field (ACE-Step's `RequestParser` unpacks it, keeping types) +
      the `ctx_audio` file. Text artifacts (lyrics/audio_codes/metadata) now
      preview inline in a `<pre>`. Params sourced from the fork's
      `GenerateMusicRequest` model. Needs the real backend on the box to verify.

**Part C done** for all three implemented backends (demucs verified on the mock;
SA3 + ACE-Step forms ready to exercise once each is up on the GPU box).

## Later — The rest

### Web console polish

- [ ] **Custom audio widget with a waveform**, in the style of SlopBC's player —
      replace the stock `<audio controls>` on artifact cards with a proper
      waveform view + transport. Keep it self-contained (no external JS lib /
      CDN, rule 5): render the waveform ourselves from the decoded PCM
      (WebAudio `decodeAudioData` → downsample peaks → `<canvas>`), with
      play/scrub over the same buffer. Look at SlopBC's widget for the visual
      style to match. Applies to every service's test page.
- [x] **Show the spectrogram image inline** instead of just a download link —
      DONE (renderArtifacts renders `<img>` for any `image/*` artifact). Left
      here as the anchor for any follow-up (click-to-zoom / lightbox, sizing
      controls) if we want it.

### Artifact lifetime & cleanup

Today **nothing is ever cleaned up**. Harvested artifacts live forever in ASS's
results store (`<results_dir>/<job_id>/<name>` + sqlite `artifacts` rows) with no
TTL/prune/delete anywhere in the code, AND a duplicate copy accumulates on each
backend's bind-mounted `/app/outputs` (`/srv/ass/outputs/<svc>`) because ASS
harvests over HTTP but never tells the backend to delete. On a busy box that's
unbounded disk growth in two places. (Inputs are fine — backends unlink their
temp upload after the job; ASS buffers uploads in memory, not disk.)

- [ ] **Retention policy for the ASS results store.** Age- and/or size-based
      prune of `<results_dir>` that also deletes the matching sqlite rows (jobs +
      artifacts). Config it under a `[storage]` knob (e.g. `results_ttl`,
      `results_max_gb`); off by default (the big server keeps everything), on for
      constrained boxes — mirrors the `idle_ttl` philosophy. One-dir-per-job
      layout makes each delete a single `RemoveAll`.
- [ ] **`DELETE /v1/jobs/{id}` on ASS** — no way to remove a job/its artifacts
      today. Add the endpoint (RemoveAll the job dir + delete the sqlite rows),
      and a delete button on the admin/test pages. Lease-safe: refuse (or defer)
      while a job is in flight, same discipline as the operator controls.
- [ ] **Kill the duplicate backend-side copy.** ASS never calls the backend's
      `DELETE /v1/jobs/{id}`, so results pile up on the mounted outputs dir too.
      Either (a) have the engine call the backend's DELETE right after a
      successful harvest, or (b) drop the `/app/outputs` bind-mount entirely —
      per CLAUDE.md it's optional since ASS reads results over HTTP, so (b) is the
      simpler fix and removes the orphaned-files-after-`evict=stop` problem. Lean
      (b), keep (a) as the option if a backend needs scratch space on disk.

### Backends & orchestration

- [x] **Preload all** — DONE (2026-09-19). `POST /v1/backends/preload-all` +
      admin button: cold-start and park every parkable backend that fits,
      biggest first, never stopping anything. Also the first place
      `memory.ram_budget_mb` is enforced. See docs/scheduling.md.

- [ ] **Arbiter fairness: a busy backend can starve a waiter forever.** The
      pinned fast path hands out leases without checking whether another
      service is waiting to evict, so a steady stream of ACE-Step jobs keeps
      YuE in `queued` until its 30-minute job timeout fails it. Fix sketch (a
      per-backend `draining` flag, or FIFO tickets) and the related no-FIFO and
      swap-rollback gaps are written up in
      [docs/scheduling.md](docs/scheduling.md) under "Known gaps". Decide before
      the server has more than one user.

- [x] **Repoint SlopBC at ASS for ACE-Step** — DONE (2026-09-18), and Stable
      Audio with it (Demucs was already through ASS). SlopBC's `internal/engine`
      now speaks ASS's unified envelope (`POST /v1/acestep/jobs` +
      `GET /v1/jobs/{id}`), reassembling ACE's rich per-take metadata from the
      fork's `metadata.json`/`lyrics.txt`/`audio_codes.txt` artifacts; the
      cover/repaint multipart path and `audio_codes` reuse are preserved. Two new
      ASS features landed to support it:
      - **`GET /v1/backends/{service}/info`** — read-only passthrough of a
        backend's `/v1/info` (warms it to answer). SlopBC needs it for the Stable
        Audio LoRA load order, which the job/`/v1/backends` surface didn't expose.
      - **`[services.x].command`** — override a backend's container CMD (the
        docker layer already had `RunSpec.Cmd`; now it's config). This is how
        Stable Audio loads finetune LoRAs at startup (`--lora-ckpt-path`), the
        way its standalone compose did; LoRAs live under `/srv/ass/loras/<svc>`.
      Two accepted fidelity losses noted in SlopBC's TODO: no per-step progress
      (ASS reports coarse state), and a batch's takes share ACE's one
      `metadata.json` so per-take seeds collapse to the joined `seed_value`.
- [x] **YuE fork: randomize a missing seed.** DONE in YuE-inference-server
      (`app/pipeline_runner.py`): `run_generation` rolls a real seed when the
      request omits one, so "blank = random" is true at the backend and the pick
      lands in the job's request.json. Was: `SongRequest.seed` defaulted to the
      constant `831001`, so a seedless request rendered the same song byte-for-byte.
      The tester's client-side stopgap can come out once the new image is deployed.
      NB ACE-Step has the same class of trap — see SlopBC's notes.
- [x] Prepare forced-aligner for ASS: add serial async
      jobs, alignment.json harvesting, VRAM telemetry, one-language residency,
      stop eviction, container defaults/cache wiring, release workflow and tests.
      ASS config estimates: 12000 MiB VRAM / 6000 MiB RAM; not live measurements.
      Synchronous `/align` restored for standalone use, sharing the same worker
      and limits, returning timing JSON directly with temporary-file cleanup.
- [x] ASS aligner hosting: production registration plus `/test/aligner` upload/
      lyrics/language form, mock alignment JSON and dev config. API integration
      verifies exact multipart forwarding, telemetry, harvest lease protection
      and results after stop. Docker smoke test exercises actual container
      startup, job completion, harvesting and stop without GPU dependencies.
- [x] Add `/park` + `/unpark` to the forced-aligner fork (model ↔ CPU RAM +
      empty_cache), advertise `eviction: "park"` + `parked` in `/v1/info`, with a
      defensive on-device restore in `_get_model`. Tests + ruff green (43 pass).
      ASS side flipped aligner to `evict = "park"` (priority 10, `no_preload`,
      `vram_parked_mb = 500` estimate) in both TOMLs; docs updated.
- [ ] Release/build the prepared forced-aligner image and smoke-test on the GPU
      server; **confirm the parked context tax** (est. 500 MiB) and pinned/RAM
      budgets on short/long tracks and language swaps. Runbook: `docs/aligner.md`;
      backend source: `child_services/forced-aligner`.
- [ ] Onboard remaining backend (Whisper)
- [ ] `idle_ttl` parked→stopped RAM reclaim (the peasant path)
- [ ] SSE job streaming instead of poll-only
- [ ] Auth (backends already support an API key; decide if ASS fronts it)
- [ ] Real weight-size measurements → concrete budget defaults. **Started** —
      see [docs/measurements.md](docs/measurements.md). First data (RTX 4080 /
      16GB box `ai2.lemon.com`): ~0.39GB VRAM baseline with everything off;
      demucs `htdemucs_ft` ~0.5GB resident, ~1.6GB peak. Demucs is small. Still
      need: measure the real parked context tax, and other models.
- [x] **Eviction priority knob.** Per-service `priority` int (higher = evicted
      last). Victim = lowest-priority zero-lease resident, ties broken LRU; all
      equal (default 0) = plain LRU, so nothing changes unless you set it. Wired
      into `planLocked`, documented in `docs/scheduling.md` ("Eviction priority"),
      README config table, CLAUDE.md, and seeded into both TOMLs. A *bias*, not a
      pin — a hard "never auto-evict" is still the sticky-flag idea below.
- [ ] **Smarter eviction than "evict on count."** The strong case: a small
      service (tiny VRAM footprint) shouldn't be evicted at all just because a
      different model wants the card — it can ride along. Once eviction is
      VRAM-budget-driven (not a pin count), the arbiter should keep a **fit-set**
      resident and only evict when the newcomer genuinely doesn't fit, preferring
      to evict big models over small ones. Consider a per-service "sticky"/pin
      flag so cheap always-useful backends (e.g. an aligner) stay resident
      indefinitely. Depends on the deferred VRAM budgeting + real weight sizes
      above.
