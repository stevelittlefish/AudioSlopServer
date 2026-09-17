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

- [ ] ass.toml env updated for all three services
- [ ] comment block rewritten
- [ ] `go test ./internal/config/` green (it parses/validates ass.toml)

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
- [ ] **ACE-Step** (`references/ACE-Step-1.5-inference-server/Dockerfile`) — uses
      `uv sync --frozen`; copy `pyproject.toml` + `uv.lock` first.
- [ ] **Stable Audio 3** (`~/git/stable-audio-3-docker/Dockerfile`) — check its
      installer (pip/uv) and copy its manifest(s) first.
- [ ] **stem-separator / demucs** (`~/git/stem-separator/Dockerfile`) — same.

Costs one more big build each (the deps layer changes once), then every future
source change pushes/pulls seconds. Verify the build still succeeds; if a
manifest references a local package/path, copy the minimum needed for the install
step (may need `README.md` or a `src/` stub — adjust per repo).

While in each Dockerfile, make them **logical/consistent** (the "dockerfiles are
logical" ask):
- [ ] Sane, documented `HF_HOME`/`TORCH_HOME` defaults (generic, e.g.
      `/cache/huggingface`; ASS overrides per-service — say so in a comment).
- [ ] `EXPOSE` matches the real serving port (ACE-Step is 2766 now, not 8001).
- [ ] Drop dead ENV / stale mount comments (e.g. ACE-Step's old
      `/app/checkpoints` run-example, `/root/.cache/huggingface` leftovers).

### 3. Stable Audio 3 — announce weight downloads, download on startup

Symptom: on first run SA3 appears to hang because it downloads a multi-GB
checkpoint lazily (on first inference) with no log output.

- [ ] Find where SA3 loads/downloads its model (likely `run_api.py` /
      whatever `from_pretrained`/`snapshot_download` it calls; search the repo).
- [ ] Move the download/load to **startup** (before the server reports ready /
      before `/health` returns 200), not first inference.
- [ ] Add clear log lines around it: e.g. `"[startup] downloading weights (…) —
      this can take several minutes on first run"` and `"[startup] weights ready"`.
      Prefer enabling HF's own progress output over silence.
- [ ] Sarcasm welcome in the log copy (CLAUDE.md), but it must be *informative*.

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
- [ ] **DCW**: old compose forced `ACESTEP_DCW_ENABLED=true` via a local patch to
      `inference.py`; our upstream merge dropped that patch for the per-model
      default (turbo → DCW on). Confirm turbo's resolved default is ON (matches
      old behaviour). Non-turbo staying off is correct (issue #1259).
- [ ] **API key**: old compose set `ACESTEP_API_KEY=qwe123` for LAN auth. ASS
      reaches the backend on a private path, so **omit it** (no auth between ASS
      and its backend). Note this decision; don't add the key.
- [ ] **Offload under ASS**: old box needed offload because it ran 3 services
      concurrently. Under ASS only one backend is resident at a time, so ACE-Step
      owns the whole card and offload may be unnecessary (xl-turbo ~9GB + 1.7B LM
      fits a 3090). We kept it for known-good parity (~1s/render). Once it runs
      on the box, consider dropping offload for speed — measure first.

### 5. One shared token + first-run smoke test

- [ ] Document (README "Configuration") the token step: `echo <token> >
      /srv/ass/cache/hf-token` once, host-side. Update any existing note that
      says `…/huggingface/token`.
- [ ] After the three releases: on the box, delete `/srv/ass/cache/*` model data
      if you want a clean slate (weights are disposable), restart ASS, and
      confirm each service downloads into its OWN `/cache/<service>/…` and
      authenticates from the one `/cache/hf-token`.

---

## Release checklist (the one final slow build per service)

Each fork change needs a rebuilt image via its `make_release.sh` (cuts a `v*`
tag → GHCR). Order doesn't matter; do all three, then pull on the box.

- [ ] ACE-Step: `references/ACE-Step-1.5-inference-server/make_release.sh vX.Y.Z "…"`
- [ ] Stable Audio 3: `~/git/stable-audio-3-docker/make_release.sh vX.Y.Z "…"`
- [ ] stem-separator: `~/git/stem-separator/make_release.sh vX.Y.Z "…"`
- [ ] `ass.toml` committed + pushed (ASS-side, no rebuild)
- [ ] On the box: `git pull`, `docker pull …:latest` ×3, seed `/cache/hf-token`,
      restart ASS, smoke-test each service's test page (`/test/<service>`).

## Ground rules (CLAUDE.md, don't forget)

- Commit straight to `main` and **push** — announce "Slopping it straight to main!"
- Never commit the HF token. Seed it on the host only.
- Sarcasm in commits/comments; Go for anything we own; no OOP.
- All three fork clones with a configured `origin`: ACE-Step is under
  `references/`; SA3 and stem-separator are full clones under `~/git/`.
