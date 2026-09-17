# ASS — Audio Slop Server

## The Problem

Running modern audio AI means juggling a zoo of heavyweight, GPU-hungry services:

- **DEMUCS** — source separation
- **Whisper** — transcription
- **Stable Audio 3** — audio/music generation
- **YuE 2** — music generation
- **ACE-Step 1.5 XL** — music generation
- ...and more arriving all the time.

Each one wants a GPU. But GPUs are expensive, and even if you have several, you may only want to dedicate **one** of them to audio slop generation.

## The Solution

Enter the **ASS** — the **Audio Slop Server**.

ASS runs all of these disparate audio services on a **single GPU**, swapping models in and out on demand — the way [Ollama](https://ollama.com) does for LLMs. One GPU, many models, loaded and evicted as needed.

No multibillionaire budget required.

## Quickstart

On a GPU host with **Docker** + **nvidia-container-toolkit**, GPU 0 free:

```sh
git clone https://github.com/stevelittlefish/AudioSlopServer.git
cd AudioSlopServer

sudo mkdir -p /srv/ass/cache /srv/ass/data   # persistent: model cache + job store
./pull-services.sh                           # pull backend images from GHCR
docker compose up -d --build                 # start ASS on :2645

curl -s localhost:2645/health                # {"status":"ok","service":"ASS"}
```

Submit a job (ASS brings the backend up on demand, harvests the results, and
serves them from its own store):

```sh
curl -s -F 'audio=@song.wav' -F 'params={"mode":"two-stem"}' \
  localhost:2645/v1/demucs/jobs               # -> {"job_id":"..."}

curl -s localhost:2645/v1/jobs/<job_id>       # poll until "succeeded"
curl -o vocals.wav localhost:2645/v1/jobs/<job_id>/result/vocals
```

That's the whole loop. No GPU? Develop against mock backends instead:
`./scripts/build-mockbackend.sh` then `./run.sh -config ass.dev.toml`. Full
deployment detail and the park/unpark validation steps live in
[docs/deploy.md](docs/deploy.md); every config knob is in
[Configuration](#configuration).

## Best Practices & Guiding Philosophy

We hold ourselves to the highest standards. Here they are.

### The Great Philosophy of Software Languages

1. **No JavaScript on the server. Ever.** We are not failed front-end
   engineers — we are failed *back-end* engineers. That's why we make the Slop.
   (JavaScript is fine on the front-end, where it belongs, and as tooling to
   build/validate front-end code — never in committed server code.)
2. **Python is tolerated, not embraced.** Much of the AI world runs on Python,
   dragging in the whole miserable circus of virtualenvs, requirements.txt,
   pyenv, poetry, conda, uv, and a thousand other tools invented to avoid the
   system pip. We'll commit some Python where forced — it's at least better than
   JavaScript — but we avoid it where we can.
3. **Go is our language of choice.** You type `go run .` and it just works. We
   don't know how. We don't care. We get a cool binary we can run. Go for
   everything we control; Python only where the AI bastards force our hand.
4. **No fancy-pants front-end frameworks.** React and its kin are hated. If we
   ever serve pages for humans: plain JavaScript, separate URLs per page, and
   server-generated HTML. (Realistically, ASS is probably 100% API anyway.)
5. **No Object-Oriented Programming.** OOP is ideological nonsense invented by
   failed programmers with too much time on their hands. Plain functions over
   plain data.

### Objectives & Coding Conventions

1. **ASCII art on startup.** The app prints **ASS** in big ASCII-art letters to
   the log when it starts. Non-negotiable.
2. **Sarcasm required.** Commit messages and code comments carry sarcasm and
   wit. Dry humor over dry documentation.
3. **Consistent API across sub-services.** A fairly consistent API shape across
   all audio sub-services, so callers don't relearn everything per service.
   Exceptions allowed where a service genuinely needs one.
4. **Config in TOML, not environment variables.** No env vars except where
   absolutely necessary. All configuration lives in TOML files.
5. **Dependencies are expensive.** Every dependency is a cost to justify. Prefer
   the standard library; pull something in only when it genuinely earns its keep.
6. **Configurable memory footprint.** Runs on a 128GB server and on a 16GB
   laptop. Which services stay resident in RAM vs. fully unload is configured
   per service.

### Version Control

We **Slop straight to `main`** and push immediately. No branches, no PRs —
those are for people who care about their code. Slop is for the masses, and the
masses can't consume it while it's sitting on our hard drive.

## Configuration

Everything ASS knows lives in one TOML file (rule 4 — no environment-variable
swamp). Point ASS at it with `-config`:

```sh
./run.sh -config ass.toml          # bare binary
# or, in Docker, docker-compose.yml mounts ./ass.toml into the container
```

See `ass.toml` (real-ish) and `ass.dev.toml` (mock backends, GPU off) for
worked examples. The full option surface:

### Global tables

| Table / key | Type | Default | Meaning |
|---|---|---|---|
| `memory.ram_budget_mb` | int | — | Total RAM budget. The peasant box sets this low so services fall back to `stop`. |
| `gpu.enabled` | bool | `false` | Whether to hand the card to backends. **Must be `true` on a real GPU box** — false means no `--gpus` is ever requested. |
| `gpu.device` | int | `0` | Which card ASS may use. |
| `gpu.vram_budget_mb` | int | — | Card size minus headroom; covers the pinned model + each parked backend's context tax. |
| `gpu.context_tax_mb` | int | — | VRAM a parked (alive) process still holds, charged per parked backend. |
| `gpu.max_resident` | int | `1` | How many backends may hold the GPU at once. `1` = one model on the card, the whole point. |
| `server.addr` | string | `:2645` | Where ASS itself listens (`0xA55` = "ASS" in hex). |
| `storage.db_path` | string | `data/ass.db` | The sqlite job store. |
| `storage.results_dir` | string | `data/results` | On-disk harvested-artifact store. |
| `docker.socket` | string | platform default | Docker daemon socket (e.g. `/var/run/docker.sock`). |

> **Why `:2645`?** It's `0xA55` — "ASS" spelled in hex. Unique, not an `80xx`,
> no 69 or 420, and it's got a reason you'll actually remember. ASS lives at
> `0xA55`.

### Per-service: `[services.<name>]`

`<name>` is the service key used in the API path (`POST /v1/<name>/jobs`).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `image` | string | *(required)* | Docker image to run. Local (`stem-separation:local`) or a registry ref (`ghcr.io/.../stem-separator:latest`). ASS never auto-pulls — the image must be present locally. |
| `port` | int | *(required)* | Container port, **published to the same host port**, and injected into the container as `PORT`. |
| `verb` | string | — | Job verb (`separate`, `generate`, …); injected as `VERB`. Forms the backend URL `/v1/<verb>`. |
| `container` | string | `ass-<name>` | Container name ASS creates. |
| `evict` | `park`\|`stop` | `stop` | How ASS frees the GPU. `park` needs the backend's `/park`+`/unpark`; `stop` kills the container. |
| `ram_reserve_mb` | int | `0` | Cost of keeping this parked in RAM, for budgeting. |
| `idle_ttl` | duration | `0` | `0` = never reclaim parked RAM. Set e.g. `"10m"` on a constrained box to demote parked → stopped. |
| `env` | table | `{}` | Extra environment for the container. **`PORT` and `VERB` are always injected**; add anything else here (e.g. a backend that reads `SEP_PORT` instead of `PORT`). |
| `volumes` | list | `[]` | Bind mounts, Docker's `"host:container[:ro]"` syntax. Fully yours to change — see below. |
| `shm_size_mb` | int | daemon default | `/dev/shm` size; some models want more than Docker's 64MB default. |

### Changing bind mounts (weight caches, output dirs)

`volumes` is plain config — edit `ass.toml`, restart ASS, done, no rebuild. The
**host** side (left of the colon) is entirely yours; point a cache anywhere. The
**container** side (right) is dictated by the *image*, not ASS: our backend
images put caches under `/cache` (`HF_HOME=/cache/huggingface`,
`TORCH_HOME=/cache/torch`), so weights only persist if something is mounted at
`/cache`. You can override that too by setting a different `HF_HOME` in `env` and
mounting to match.

**One shared cache for all services (the convention).** Every backend image
caches under `/cache`, so point *every* service's `/cache` mount at the **same**
host directory — `/srv/ass/cache` — rather than a per-service folder:

```toml
[services.demucs]
volumes = ["/srv/ass/cache:/cache", "/srv/ass/outputs/demucs:/app/outputs"]
[services.yue]
volumes = ["/srv/ass/cache:/cache", "/srv/ass/outputs/yue:/app/outputs"]
# ...same /srv/ass/cache for all N
```

That way the model weights **and the Hugging Face token** are downloaded/written
once and shared across every backend (the HF cache dedups by repo, so shared base
models aren't stored twice). Per-service `outputs` stay separate — or drop the
outputs mount entirely, since ASS harvests results over HTTP.

### Secrets & the Hugging Face token

ASS has no secret store, and **`ass.toml` is committed — never put a token in
it.** Backends that need credentials get them one of two ways:

1. **Pre-seed the shared cache (recommended).** The HF token lives at
   `$HF_HOME/token`, and `HF_HOME` is inside the bind-mounted **shared** cache
   (`/srv/ass/cache`), so you write it **once per host** and every service is
   authenticated — not once per service:

   ```sh
   mkdir -p /srv/ass/cache/huggingface
   printf '%s' 'hf_your_token_here' > /srv/ass/cache/huggingface/token
   chmod 600 /srv/ass/cache/huggingface/token
   # equivalently: HF_HOME=/srv/ass/cache/huggingface huggingface-cli login
   ```

   Every backend sees it at `/cache/huggingface/token` and is authenticated. It
   survives container recreation and image updates — write it once, for all N
   services.

2. **Env var**, for gated models: set `HF_TOKEN` in the service's `env`. Only do
   this if your `ass.toml` is a local, uncommitted copy — otherwise the token
   leaks into git. Prefer method 1.

**demucs (`htdemucs_ft`) needs no token** — its weights are public. The token
only matters for gated models (some alignment/generation backends down the line).

## License

MIT — see [LICENSE](LICENSE). ASS talks to every backend over HTTP, across a
process boundary, and never vendors third-party source, so it stays permissive
no matter how copyleft the backends behind it are.

## Status

🐣 **Slice 1 complete — the orchestrator works end to end.** ASS boots, reads
its TOML config, brings a backend's container up on demand (via the raw Docker
socket, no SDK), forwards a job, polls it to completion, and harvests every
output — multi-file and multi-type (stems, audio, ABC scores) — into its own
store so results survive the backend being evicted. All verified on a machine
with **no GPU**, using a mock backend.

What's working today:

- `POST /v1/{service}/jobs` → submit; `GET /v1/jobs/{id}` → poll;
  `GET /v1/jobs/{id}/result[/{name}]` → download; `GET /v1/backends` → status.
- On-demand container lifecycle (create → health-wait → forward) and
  harvest-on-completion into a sqlite-tracked, on-disk results store.
- One dependency-light Go binary. `./run.sh -config ass.dev.toml`.

Next up — **Slice 2**: the actual reason ASS exists. Two backends, one GPU, and
the **arbiter** that evicts one model to load another (Ollama-style), plus real
`/park` / `/unpark` on a forked backend. See [TODO.md](TODO.md).
