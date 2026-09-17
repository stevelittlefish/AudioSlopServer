# Deploy & end-to-end test (seaslug.ai / ai.lemon.com)

How to run ASS against a **real** backend on the GPU box, all in Docker. This is
the first time the GPU path (`--gpus device=0`) and the park/unpark CUDA code
actually run — everything on the dev boxes is GPU-less.

## The mental model (read this first)

**ASS is the orchestrator. It replaces docker-compose for the backends.** ASS
talks to the host Docker daemon and starts/stops backend containers itself, one
at a time, on demand. Consequences:

- **No mega-compose file for the backends.** One compose that `up`s them all
  would fight ASS for the card. The only thing you `compose up` yourself is
  **ASS** (`docker-compose.yml` here runs ASS and nothing else).
- **No registry.** Build and run happen on the same box, so a **local image**
  (`stem-separation:local`) is exactly what ASS looks up by name. A registry only
  matters when moving images between machines.
- Each backend repo's own compose file degrades to just "how I build the image."

## Prereqs on the host

- Docker with **nvidia-container-toolkit** installed. (The *backends* get
  `--gpus`; ASS itself never opens a CUDA context, so ASS needs no GPU access.)
- **GPU 0 cleared** of other processes — `ass.toml` pins `device = 0`.
  Confirm with `nvidia-smi`.

## Step 1 — build the backend image (local, no registry)

```sh
cd stem-separator
docker build -t stem-separation:local .      # or: docker compose build
docker images stem-separation:local          # confirm it's there
```

ASS will refuse with a clear message if the image is missing, so this must exist
before you submit a job.

## Step 2 — start ASS (in Docker)

From the AudioSlopServer repo root:

```sh
mkdir -p data                       # ASS's job DB + harvested artifacts land here
docker compose up --build -d        # builds ass:local, starts it on the host network
docker compose logs -f ass          # ASCII-art banner, then "listening on :8080"
curl -s localhost:8080/health       # {"status":"ok","service":"ASS"}
curl -s localhost:8080/v1/backends  # demucs, residency "stopped" (not started yet)
```

Why host networking: ASS creates backend containers that publish their ports to
the host, and on the host network ASS reaches them at `localhost:<port>` — no
shared-network/container-name wiring needed (that's a later refinement).

## Step 3 — submit a real job, end to end

You post the multipart to **ASS** (`/v1/demucs/jobs`); ASS makes demucs resident
(cold start on first hit — Demucs downloads its weights, so the first job is
slow), forwards the work, polls, and harvests the stems into its own store.

```sh
# submit (two-stem = vocals + instrumental)
curl -s -F 'audio=@song.wav' -F 'params={"mode":"two-stem"}' \
  localhost:8080/v1/demucs/jobs
# -> {"job_id":"<ASS_ID>"}

# poll until succeeded (first run: allow minutes for the model download)
curl -s localhost:8080/v1/jobs/<ASS_ID>

# once "state":"succeeded", the job lists artifacts; download them from ASS's
# OWN store (served even after the backend is later evicted)
curl -o vocals.wav       localhost:8080/v1/jobs/<ASS_ID>/result/vocals
curl -o instrumental.wav localhost:8080/v1/jobs/<ASS_ID>/result/no_vocals

# meanwhile the backend is pinned on the card:
curl -s localhost:8080/v1/backends   # demucs residency "pinned"
nvidia-smi                            # ass-demucs holding VRAM on GPU 0
```

Success = two real WAVs, correct sizes, and `/v1/jobs/<id>` listing them as
`artifacts` with `content_type: audio/wav`.

## Step 4 — validate park/unpark on the real card (the risky new code)

With only demucs configured, the arbiter never evicts (nothing else wants the
card), so `/park` won't fire on its own. Test it **directly** against the backend
(its port is published to the host) — this exercises exactly the code that has
never run on CUDA: `separator.model.to('cpu')` + `empty_cache()`.

```sh
nvidia-smi --query-gpu=memory.used --format=csv,noheader   # note the pinned figure
curl -s -XPOST localhost:5336/park                          # {"parked":true}
nvidia-smi --query-gpu=memory.used --format=csv,noheader   # should DROP a lot
curl -s localhost:5336/health                               # {"status":"ok","parked":true}
curl -s -XPOST localhost:5336/unpark                        # {"parked":false}
nvidia-smi --query-gpu=memory.used --format=csv,noheader   # back up
```

**Two things to check here:**
1. The VRAM actually drops on park (if it doesn't, `empty_cache()` isn't freeing —
   or the weights didn't move). The residual VRAM *while parked* is the real
   **context tax** — record it in [measurements.md](measurements.md); it's still
   a guess in `ass.toml` (`context_tax_mb = 500`).
2. That `park_model` found the model: it assumes `demucs.api.Separator` exposes
   `.model`. If park 500s, that attribute is wrong for the installed demucs
   version — a one-line fix in `stem-separator/app/separation.py`.

## Cleanup

```sh
docker compose down            # stops ASS
docker rm -f ass-demucs        # ASS's backend container (ASS names it ass-<service>)
```

## Model weights persist across updates (no re-downloads)

ASS doesn't use a backend's own compose file, but it **replicates the same bind
mounts** from `ass.toml`'s per-service `volumes` (→ docker `HostConfig.Binds`).
For demucs that's `/srv/stem-separation/cache:/cache`, matching the image's
`HF_HOME`/`TORCH_HOME`.

Because the weight cache lives on the **host**, not in the container or image:

- Recreating the container (image update, `stop`-eviction restart, config change)
  re-mounts the same host dir — cache intact, **no re-download**.
- Pulling a new image version doesn't touch the host dir; weights are keyed by
  model name, so any image version finds them.
- The only download that ever happens is the **first** job on a **fresh host**.

Don't wipe `/srv/stem-separation` on the box. Nothing to pre-create — Docker
makes the source dir on first run.

## Notes / gotchas

- **Bare-binary alternative** (no ASS image): `./run.sh -config ass.toml` if Go is
  installed on the host. Same behaviour; the Docker path is preferred for deploy.
- **First job is slow**: Demucs fetches `htdemucs_ft` from HuggingFace on first
  use. `ass.toml` mounts `/srv/stem-separation/cache` so it's a one-time cost;
  make sure that host dir is writable.
- **A real swap** (evict-one/load-the-other on the actual GPU) needs a *second*
  real backend in `ass.toml`. Until then this proves the single-backend vertical
  path + that park genuinely frees VRAM.
