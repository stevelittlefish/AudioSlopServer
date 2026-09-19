# DEPLOY — rebuild the backends and ship to the box

Runbook for the FIXNOW cache/Dockerfile work (see
[`FIXNOW-cache-and-dockerfiles.md`](FIXNOW-cache-and-dockerfiles.md)). Everything
that could be done from this GPU-less dev box **is already committed and pushed**
to `main` on all four repos. What's left is: cut three image releases, then pull +
smoke-test on the GPU box.

## What's already done (no action needed)

- **ASS** (`ass.toml`, `README.md`): per-service cache env + one shared
  `HF_TOKEN_PATH=/cache/hf-token`, comments rewritten. Committed + pushed. No image.
- **ACE-Step fork**: Dockerfile reordered deps-before-source (kills the ~30-min
  re-push), per-service cache defaults, stale comments dropped. Committed + pushed.
- **Stable Audio 3 fork**: startup now announces the weight download instead of
  looking hung; `HF_TOKEN_PATH` default. Committed + pushed.
- **stem-separator fork**: `HF_TOKEN_PATH` default. Committed + pushed.

The two backend Dockerfiles that already did deps-before-source (SA3,
stem-separator) were left as-is; only ACE-Step needed the reorder.

## Step 1 — cut the three releases (on this box or any dev machine)

Each `make_release.sh <version> <message>` cuts an annotated tag and pushes it;
the push fires that repo's CI, which builds the image and publishes it to GHCR.
The builds run in CI — you don't need a GPU for them. Bump all three to **v1.1.0**:

```sh
cd ~/git/AudioSlopServer/child_services/ACE-Step-1.5-inference-server
./make_release.sh v1.1.0 "Fast deps-before-source Dockerfile; per-service cache + shared HF_TOKEN_PATH"

cd ~/git/stable-audio-3-docker
./make_release.sh v1.1.0 "Announce weight download at startup; HF_TOKEN_PATH"

cd ~/git/stem-separator
./make_release.sh v1.1.0 "HF_TOKEN_PATH default"
```

Then watch the three CI runs go green (GitHub → each repo → Actions) before
pulling on the box. GHCR publishes to:

- `ghcr.io/stevelittlefish/ace-step-1.5-inference-server:latest`
- `ghcr.io/stevelittlefish/stable-audio-3-docker:latest`
- `ghcr.io/stevelittlefish/stem-separator:latest`

## Step 2 — on the GPU box (`ai.lemon.com`)

```sh
# 2a. Get the new ass.toml (README/config only — ASS itself doesn't need a rebuild
#     unless you changed Go, which this work order didn't).
cd ~/git/AudioSlopServer && git pull

# 2b. Seed the ONE shared HF token (only if not already present, or if you're
#     starting from a clean cache). This replaces the old huggingface/token file.
mkdir -p /srv/ass/cache
printf '%s' 'hf_your_token_here' > /srv/ass/cache/hf-token
chmod 600 /srv/ass/cache/hf-token

# 2c. OPTIONAL clean slate. Weights are disposable; the new per-service layout
#     re-downloads into /cache/<service>/ on first run. Do this if you want to
#     prove the new layout from scratch (skip to keep existing downloads — though
#     old weights sat under the shared layout, so services WILL re-download into
#     their new subdirs regardless on first run).
#     Keep the token! Only nuke the model dirs:
rm -rf /srv/ass/cache/demucs /srv/ass/cache/stableaudio /srv/ass/cache/acestep
#     (Old shared cache leftovers you no longer need: /srv/ass/cache/huggingface,
#      /srv/ass/cache/torch — safe to remove once the new layout is confirmed.)

# 2d. Pull the three fresh images.
./pull-services.sh          # pulls all services' :latest per ass.toml
#     Reads the image list from the ASS binary (same config parse the server
#     uses; disabled services are skipped). Defaults to the ass:local container
#     (so a Docker-only host works — build it first with docker compose build),
#     falling back to `go run` if the image isn't built. The config is bind-mounted
#     by absolute path, so it can live anywhere. Force one with ASS_IMAGES_VIA=go|docker.

# 2e. Restart ASS.
./run.sh -config ass.toml   # or however ASS is (re)started on the box
```

## Step 3 — smoke-test each service

Hit each service's bespoke test page and run one job. The **first** job per
service downloads its weights into its own `/cache/<service>/…` — expect a wait,
and for SA3 you'll now see the `[startup] downloading … NOT hung … grab a coffee`
line in the logs instead of silence.

```
http://<box>:2645/test/demucs
http://<box>:2645/test/stableaudio
http://<box>:2645/test/acestep
```

Confirm on the host afterwards that each service populated its OWN subdir and that
there's exactly one token file:

```sh
ls -la /srv/ass/cache/            # hf-token + demucs/ stableaudio/ acestep/
ls /srv/ass/cache/acestep/        # huggingface/ torch/ checkpoints/
```

## If something's wrong

- **SA3 "hangs" on first job** — it's downloading; check the logs for the
  `[startup] downloading` line. It only looks hung; give it a few minutes.
- **Auth / gated-model 401** — the token file is missing or wrong. It must be at
  `/srv/ass/cache/hf-token` (host) → `/cache/hf-token` (container), and every
  service's `HF_TOKEN_PATH` points there. demucs needs no token (public weights).
- **ACE-Step re-downloading every boot** — check `ACESTEP_CHECKPOINTS_DIR` resolves
  to `/cache/acestep/checkpoints` (it does in `ass.toml`) and that `/srv/ass/cache`
  is actually mounted.

Once all three services pass on the box, the FIXNOW work is fully released —
delete `FIXNOW-cache-and-dockerfiles.md` (and this file, if you like) or fold any
durable notes into `TODO.md`.
