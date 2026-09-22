# Configuration

Everything ASS knows lives in one TOML file (philosophy rule 4 — no
environment-variable swamp). Point ASS at it with `-config`:

```sh
./run.sh -config ass.toml          # bare binary
# or, in Docker, docker-compose.yml mounts ./ass.toml into the container
```

See [`ass.toml`](../ass.toml) (real-ish) and [`ass.dev.toml`](../ass.dev.toml)
(mock backends, GPU off) for worked examples. The full option surface follows.

## Global tables

| Table / key | Type | Default | Meaning |
|---|---|---|---|
| `memory.ram_budget_mb` | int | — | Total RAM budget. The peasant box sets this low so services fall back to `stop`. |
| `gpu.enabled` | bool | `false` | Whether to hand the card to backends. **Must be `true` on a real GPU box** — false means no `--gpus` is ever requested. |
| `gpu.device` | int | `0` | Which card ASS may use. |
| `gpu.vram_budget_mb` | int | — | Card size minus headroom; covers the pinned model + each parked backend's context tax. |
| `gpu.context_tax_mb` | int | — | VRAM a parked (alive) process still holds, charged per parked backend. |
| `gpu.max_resident` | int | `1` | How many backends may hold the GPU at once. `1` = one model on the card, the whole point. |
| `server.addr` | string | `:2645` | Where ASS itself listens (`0xA55` = "ASS" in hex). `:2645` binds all interfaces (0.0.0.0). |
| `web.enabled` | bool | `true` | Serve the web console + operator controls (park/stop/unload-all by hand). Shares the `server.addr` listener; **no auth yet**, so keep ASS off the open internet. Set `false` for a strictly API-only box. |
| `storage.db_path` | string | `data/ass.db` | The sqlite job store. |
| `storage.results_dir` | string | `data/results` | On-disk harvested-artifact store. |
| `retention.max_total_mb` | int | `15000` (~15 GB) | Total on-disk size cap for harvested results. Over it, the reaper deletes oldest finished jobs (rows **and** files) until back under. The knob that matters for audio (~80 MB/job, ~8 GB per 100 jobs). **Defaults ON** — a config with no `[retention]` table still tidies up; set `max_total_mb = 0` to hoard forever. |
| `retention.max_jobs` | int | `0` (off) | Keep at most this many finished jobs; oldest deleted beyond it. |
| `retention.max_age` | duration | `0` (off) | Delete finished jobs older than this (e.g. `"720h"` = 30 days). |
| `retention.sweep_interval` | duration | `10m` | How often the reaper enforces the limits. It also sweeps at startup and right after each job finishes. Ignored when every limit is `0` (the reaper never starts). |
| `docker.socket` | string | platform default | Docker daemon socket (e.g. `/var/run/docker.sock`). |

> **Why `:2645`?** It's `0xA55` — "ASS" spelled in hex. Unique, not an `80xx`,
> no 69 or 420, and it's got a reason you'll actually remember. ASS lives at
> `0xA55`.

## Per-service: `[services.<name>]`

`<name>` is the service key used in the API path (`POST /v1/<name>/jobs`).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `disabled` | bool | `false` | Remove this service from ASS entirely: not registered, not startable, absent from `/v1/backends` and the web console, and skipped by `pull-services.sh`. Its other fields aren't validated, so a disabled block may be incomplete. |
| `image` | string | *(required)* | Docker image to run. Local (`stem-separation:local`) or a registry ref (`ghcr.io/.../stem-separator:latest`). ASS never auto-pulls — the image must be present locally. |
| `port` | int | *(required)* | Container port, **published to the same host port**, and injected into the container as `PORT`. |
| `verb` | string | — | Job verb (`separate`, `generate`, …); injected as `VERB`. Forms the backend URL `/v1/<verb>`. |
| `container` | string | `ass-<name>` | Container name ASS creates. |
| `evict` | `park`\|`stop` | `stop` | How ASS frees the GPU. `park` needs the backend's `/park`+`/unpark`; `stop` kills the container. |
| `priority` | int | `0` | Which resident is evicted first when the card is full. **Higher = evicted last.** Victim = lowest-priority zero-lease resident, ties broken least-recently-used; all equal (the default) = plain LRU. |
| `no_preload` | bool | `false` | Exclude this service from "Preload all" (`POST /v1/backends/preload-all`). It stays stopped until a real job wants it. For rarely-used backends or RAM hogs you'd rather not warm eagerly. (`stop` services never preload anyway.) |
| `ram_reserve_mb` | int | `0` | Cost of keeping this parked in RAM, for budgeting. |
| `idle_ttl` | duration | `0` | `0` = never reclaim parked RAM. Set e.g. `"10m"` on a constrained box to demote parked → stopped. |
| `env` | table | `{}` | Extra environment for the container. **`PORT` and `VERB` are always injected**; add anything else here (e.g. a backend that reads `SEP_PORT` instead of `PORT`). |
| `volumes` | list | `[]` | Bind mounts, Docker's `"host:container[:ro]"` syntax. Fully yours to change — see below. |
| `command` | list | `[]` | Override the image's default CMD. **Replaces it entirely**, so repeat the defaults. This is how Stable Audio loads finetune LoRAs at startup — see below. |
| `shm_size_mb` | int | daemon default | `/dev/shm` size; some models want more than Docker's 64MB default. |

## Loading finetune LoRAs (Stable Audio)

A LoRA lives in the **backend**, not ASS. Our Stable Audio fork loads one by
overriding the container command with `run_api.py`'s `--lora-ckpt-path`, exactly
as its standalone `compose.yaml` did. Stage the `.safetensors` under
`/srv/ass/loras/<service>` (mounted read-only at `/loras`), then set `command`:

```toml
[services.stableaudio]
volumes = ["/srv/ass/cache:/cache", "/srv/ass/outputs/stableaudio:/app/outputs", "/srv/ass/loras/stableaudio:/loras:ro"]
command = [
  "/app/.venv/bin/python", "/app/run_api.py",
  "--model", "medium",
  "--lora-ckpt-path", "/loras/limp-bizkit-4k.safetensors",   # index 0
]
```

Load **order** sets each LoRA's index in the `/v1/generate` `loras` array (first
= index 0) — the same order a client reads back from `/v1/backends/stableaudio/info`.

## Changing bind mounts (weight caches, output dirs)

`volumes` is plain config — edit `ass.toml`, restart ASS, done, no rebuild. The
**host** side (left of the colon) is entirely yours; point a cache anywhere. The
**container** side (right) is dictated by the *image*, not ASS: our backend
images default their caches under `/cache` (`HF_HOME=/cache/huggingface`,
`TORCH_HOME=/cache/torch`), and `ass.toml` overrides those per service in `env`
(see below), so weights only persist if something is mounted at `/cache`.

**One shared mount, a private subdir per service (the convention).** Every
service still mounts the **same** host directory — `/srv/ass/cache` — at `/cache`,
but each one's `env` points `HF_HOME`/`TORCH_HOME` at its **own** subdir so a
service's weights are one deletable folder (`rm -rf /srv/ass/cache/demucs` and it
re-downloads):

```toml
[services.demucs]
env     = { HF_HOME = "/cache/demucs/huggingface", TORCH_HOME = "/cache/demucs/torch", HF_TOKEN_PATH = "/cache/hf-token" }
volumes = ["/srv/ass/cache:/cache", "/srv/ass/outputs/demucs:/app/outputs"]
[services.stableaudio]
env     = { HF_HOME = "/cache/stableaudio/huggingface", TORCH_HOME = "/cache/stableaudio/torch", HF_TOKEN_PATH = "/cache/hf-token" }
volumes = ["/srv/ass/cache:/cache", "/srv/ass/outputs/stableaudio:/app/outputs"]
# ...same /srv/ass/cache mount for all N, different subdir each
```

Why not one shared HF cache? These backends share no base weights, so the dedup a
shared cache would buy is ~nothing, and its content-addressed blobs make selective
deletion error-prone. Per-service folders are trivially disposable. The **one**
thing that stays shared is the HF token — a single file every service reads via
`HF_TOKEN_PATH` (next section). Per-service `outputs` stay separate — or drop the
outputs mount entirely, since ASS harvests results over HTTP.

## Secrets & the Hugging Face token

ASS has no secret store, and **`ass.toml` is committed — never put a token in
it.** Backends that need credentials get them one of two ways:

1. **Pre-seed the ONE shared token file (recommended).** Because each service now
   has its own `HF_HOME`, its own per-service token file would be a pain. Instead
   keep a single token file and point every service at it with `HF_TOKEN_PATH`
   (`huggingface_hub` reads that env var; `ass.toml` sets it to `/cache/hf-token`
   on all services). Write it **once per host**:

   ```sh
   mkdir -p /srv/ass/cache
   printf '%s' 'hf_your_token_here' > /srv/ass/cache/hf-token
   chmod 600 /srv/ass/cache/hf-token
   ```

   Every backend sees it at `/cache/hf-token` (via `HF_TOKEN_PATH`) and is
   authenticated. It survives container recreation and image updates — write it
   once, for all N services.

2. **Env var**, for gated models: set `HF_TOKEN` in the service's `env`. Only do
   this if your `ass.toml` is a local, uncommitted copy — otherwise the token
   leaks into git. Prefer method 1.

**demucs (`htdemucs_ft`) needs no token** — its weights are public. The token
only matters for gated models (some alignment/generation backends down the line).
