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

**Part C done** for stem-separator; the generate form is ready to exercise once
SA3 is verified on the GPU box.

Build generic-first (one test page for all services), add per-service polish
after. Do Part A first — it's independently useful and unblocks B and C.

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
