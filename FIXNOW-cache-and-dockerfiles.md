# FIX NOW — uniform caching, one HF token, sane fast Dockerfiles

A focused work order, written to be executed in a **fresh session** with no prior
context. Linked from [`TODO.md`](TODO.md). When every box below is ticked and
released, delete this file (or fold the durable notes into `TODO.md`).

## Why this exists

Onboarding ACE-Step exposed that our three backends cache model weights three
different ways, the Docker builds re-push multi-GB layers on every code change,
Stable Audio 3 looks hung while it silently downloads weights, and ACE-Step's
defaults drifted from the old known-good `docker-compose`. This cleans all of it
up in **one final slow rebuild per service**, after which builds are fast.

## Decisions already made (do not re-litigate)

- **Per-service subdirectory, NOT one shared HuggingFace cache.** Each service
  owns `/cache/<service>/…`. Reason: the goal is uniform layout + trivial
  retirement (`rm -rf /cache/<service>`). These services share no base weights,
  so the dedup a shared HF cache would buy is worth ~nothing, and shared-cache
  deletion is error-prone (content-addressed blobs, services pulling multiple
  repos). Per-service wins for us.
- **One HF token, shared, via `HF_TOKEN_PATH`.** Per-service `HF_HOME` would
  normally mean per-service token files. Avoid that: keep ONE token file and
  point every service at it with `HF_TOKEN_PATH` (huggingface_hub reads it;
  default is `$HF_HOME/token`, we override). Token lives on the host at
  `/srv/ass/cache/hf-token`, mounted (via the existing `/srv/ass/cache:/cache`)
  at `/cache/hf-token`. **Never commit the token** (CLAUDE.md) — seed it on the
  host once.
- **Model weights are disposable.** Do not preserve or migrate existing weights.
  Re-downloading on first run is fine and expected.

## Target end state

```
/srv/ass/cache/                 (host)  ->  /cache/  (in every container)
├── hf-token                    # the ONE token file, HF_TOKEN_PATH=/cache/hf-token
├── demucs/
│   ├── huggingface/            # HF_HOME
│   └── torch/                  # TORCH_HOME
├── stableaudio/
│   ├── huggingface/
│   └── torch/
└── acestep/
    ├── huggingface/            # HF_HOME (transformers bits: tokenizer, text enc)
    ├── torch/
    └── checkpoints/            # ACESTEP_CHECKPOINTS_DIR (its main DiT/VAE/LM)
```

Every service: its data is one deletable folder under `/cache/`. One token file
for all of them.

---

## Work items

### 1. ASS — per-service cache env in `ass.toml`  (repo: this one)

For each `[services.*]` `env`, add the cache roots (override the images' generic
defaults). Keep existing keys (`SEP_PORT`, `ACESTEP_API_PORT`, etc.).

- **demucs**: `HF_HOME=/cache/demucs/huggingface`, `TORCH_HOME=/cache/demucs/torch`,
  `HF_TOKEN_PATH=/cache/hf-token`
- **stableaudio**: `HF_HOME=/cache/stableaudio/huggingface`,
  `TORCH_HOME=/cache/stableaudio/torch`, `HF_TOKEN_PATH=/cache/hf-token`
- **acestep**: `HF_HOME=/cache/acestep/huggingface`,
  `TORCH_HOME=/cache/acestep/torch`, `ACESTEP_CHECKPOINTS_DIR=/cache/acestep/checkpoints`
  (already set), `HF_TOKEN_PATH=/cache/hf-token`

Rewrite the shared-cache comment block near the top of the services section to
describe the new per-service + shared-token layout (the current comments still
say "every service mounts the same /cache … token written once at $HF_HOME/token"
— update to the `HF_TOKEN_PATH` scheme above). The bind mount stays
`/srv/ass/cache:/cache` for every service — only the subdirs differ.

- [x] ass.toml env updated for all three services
- [x] comment block rewritten
- [x] `go test ./internal/config/` green (it parses/validates ass.toml)

### 2. Dockerfiles — deps before source (all three forks)

Root cause of the 30-min push/pull: each Dockerfile does `COPY . /app/` **then**
installs deps, so any source edit invalidates the huge torch/CUDA layer and it
gets rebuilt + re-pushed + re-pulled. Reorder to the standard pattern:

```dockerfile
COPY pyproject.toml uv.lock /app/     # or requirements.txt — just the manifests
RUN uv sync --frozen --no-dev ...      # big, stable, cached layer
COPY . /app/                           # source — a tiny layer
```

Do this for:
- [x] **ACE-Step** (`child_services/ACE-Step-1.5-inference-server/Dockerfile`) — was the
      real offender. Now copies `pyproject.toml` + `uv.lock` + the `nano-vllm` path
      dep first, `uv sync --no-install-project`, THEN source + a final project sync.
- [x] **Stable Audio 3** (`~/git/stable-audio-3-docker/Dockerfile`) — already did
      deps-before-source (manifests + flash-attn wheel, then `COPY .`). No change needed.
- [x] **stem-separator / demucs** (`~/git/stem-separator/Dockerfile`) — already did
      deps-before-source (torch + requirements.txt, then `COPY .`). No change needed.

Costs one more big build each (the deps layer changes once), then every future
source change pushes/pulls seconds. Verify the build still succeeds; if a
manifest references a local package/path, copy the minimum needed for the install
step (may need `README.md` or a `src/` stub — adjust per repo).

While in each Dockerfile, make them **logical/consistent** (the "dockerfiles are
logical" ask):
- [x] Sane, documented `HF_HOME`/`TORCH_HOME` defaults (generic `/cache/huggingface`;
      commented as ASS-overridden per-service). All three now also default
      `HF_TOKEN_PATH=/cache/hf-token` (the one shared token).
- [x] `EXPOSE` matches the real serving port (ACE-Step `EXPOSE 7860 2766` — 2766 is
      the API port; SA3 5335; stem-separator 5336). All correct.
- [x] Dropped dead ENV / stale mount comments (ACE-Step's `/app/checkpoints` +
      `/root/.cache/huggingface` run-examples and mkdir, rewritten HF comment).

### 3. Stable Audio 3 — announce weight downloads, download on startup

Symptom: on first run SA3 appears to hang because it downloads a multi-GB
checkpoint lazily (on first inference) with no log output.

- [x] Found it: `run_api.py` calls `StableAudioModel.from_pretrained(args.model)`.
- [x] Already at **startup** — the load runs before `uvicorn.run`, so `/health`
      only serves once weights are ready. The problem was silence, not timing.
- [x] Added clear log lines: a "downloading multi-GB, NOT hung, grab a coffee" line
      before the load and a "weights ready" line after; stop suppressing HF's own
      progress bars (`HF_HUB_DISABLE_PROGRESS_BARS` popped).
- [x] Sarcasm present and informative (CLAUDE.md).

This is a fork change → new SA3 release after.

### 4. ACE-Step — lock defaults to the old `docker-compose`

Mostly done this session; **verify** against the retired compose
(`~/docker/ACE-Step-1.5/docker-compose.yml`, quoted in the git history / prior
session) and finish:

Already committed to the fork + ass.toml:
- [x] Default DiT `acestep-v15-xl-turbo`, LM `acestep-5Hz-lm-1.7B` (Dockerfile).
- [x] `MODEL_REPO_MAPPING` now resolves the `xl-*` repos (was missing → would
      have pulled the unified repo).
- [x] `ACESTEP_LM_BACKEND=pt`, `ACESTEP_OFFLOAD_TO_CPU=true`,
      `ACESTEP_LM_OFFLOAD_TO_CPU=true`, `ACESTEP_INIT_LLM=true` (ass.toml).
- [x] Honors `ACESTEP_CHECKPOINTS_DIR` on the API startup path (was hardcoded).
- [x] Port default 2766 (0xACE), not 8001.

Verify / decide:
- [x] **DCW**: confirmed. `inference.py:156-162` resolves DCW per-model — enabled
      for Turbo, disabled for non-Turbo. Turbo is our default DiT, so DCW is ON =
      old behaviour. The old blanket `ACESTEP_DCW_ENABLED` patch is correctly gone.
- [x] **API key**: decided — OMIT. ASS reaches the backend on a private path; no
      auth between ASS and its backend. No `ACESTEP_API_KEY` added to ass.toml.
- [x] **Offload under ASS**: kept ON for known-good 3090 parity (~1s/render). Under
      ASS only one backend is resident at a time, so ACE-Step owns the whole card
      and offload may be unnecessary (xl-turbo ~9GB + 1.7B LM fits a 3090). Once it
      runs on the box, consider dropping offload for speed — measure first. Deferred,
      not blocking; the reasoning is already noted in ass.toml.

### 5. One shared token + first-run smoke test

- [x] Documented (README "Configuration") the token step: write it once to
      `/srv/ass/cache/hf-token`, host-side, read by every service via `HF_TOKEN_PATH`.
      Old `…/huggingface/token` note replaced.
- [ ] After the three releases: on the box, delete `/srv/ass/cache/*` model data
      if you want a clean slate (weights are disposable), restart ASS, and
      confirm each service downloads into its OWN `/cache/<service>/…` and
      authenticates from the one `/cache/hf-token`.

---

## Release checklist (the one final slow build per service)

All code changes are committed + pushed to `main` on every fork (current tags all
v1.0.0). Cutting the tag fires each repo's CI, which does the slow rebuild and
publishes to GHCR — no GPU needed for the build. **These three tag pushes are the
only thing left for a human (the release classifier blocked me from cutting them):**

- [ ] ACE-Step: `child_services/ACE-Step-1.5-inference-server/make_release.sh v1.1.0 "Fast deps-before-source Dockerfile; per-service cache + shared HF_TOKEN_PATH"`
- [ ] Stable Audio 3: `~/git/stable-audio-3-docker/make_release.sh v1.1.0 "Announce weight download at startup; HF_TOKEN_PATH"`
- [ ] stem-separator: `~/git/stem-separator/make_release.sh v1.1.0 "HF_TOKEN_PATH default"`
- [x] `ass.toml` committed + pushed (ASS-side, no rebuild)
- [ ] On the box: `git pull`, `docker pull …:latest` ×3, seed `/cache/hf-token`,
      restart ASS, smoke-test each service's test page (`/test/<service>`).

See **DEPLOY.md** for the full copy-paste rebuild + deploy runbook.

## Ground rules (CLAUDE.md, don't forget)

- Commit straight to `main` and **push** — announce "Slopping it straight to main!"
- Never commit the HF token. Seed it on the host only.
- Sarcasm in commits/comments; Go for anything we own; no OOP.
- All three fork clones with a configured `origin`: ACE-Step is under
  `child_services/`; SA3 and stem-separator are full clones under `~/git/`.
