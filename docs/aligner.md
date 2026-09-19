# Forced alignment through ASS

The backend source is maintained separately in
[stevelittlefish/forced-aligner](https://github.com/stevelittlefish/forced-aligner),
checked out locally under `child_services/forced-aligner`. Its ASS preparation
adds the async job API; clients going through ASS should use ASS's job API.
The backend also provides synchronous `/align` for standalone operation,
returning timing JSON directly while sharing the same serial worker and limits.
Use that route only when running independently of ASS's container lifecycle.

## Deployment

The prepared backend needs its first ASS-compatible image release:

```sh
cd child_services/forced-aligner
./make_release.sh v0.2.0 "ASS jobs, timing artifacts and bounded model residency"
```

After the container workflow publishes `ghcr.io/stevelittlefish/forced-aligner:latest`,
run `./pull-services.sh` in the ASS checkout on the GPU host and restart ASS with
the updated `ass.toml`. Alternatively, build that image tag locally on the host.
The service is named `aligner`, listens on 8830, and uses stop eviction.
The image contains its default TOML config; no extra host config file is needed.

All weights live under `/srv/ass/cache/aligner` through `/cache`. The shared
optional token is `/srv/ass/cache/hf-token`. Results are temporary backend files;
ASS harvests them into its own store before removing the container.

Initial reservations are **12,000 MiB VRAM and 6,000 MiB RAM**, both estimates.
Only one language model stays loaded. See [measurement notes](measurements.md)
for the assumptions and the live checks still needed. There is no park/unpark
support in this initial integration.

## Web console and local development

The admin console lists `aligner` automatically from the configuration. Open
`/test/aligner` to upload vocal audio, paste the exact lyrics (with line breaks),
and optionally set the language code. The page submits through ASS, polls the
job, and previews/downloads `alignment.json`. Blank language uses the backend
configuration, which defaults to English.

`ass.dev.toml` includes the mock aligner on port 8830. Rebuild the mock image
after updating ASS, then start the development config:

```sh
./scripts/build-mockbackend.sh
./run.sh -config ass.dev.toml
```

The mock returns a fixed English "Hello world" timing fixture with one unresolved
word. It does not align the uploaded audio; it exercises the upload/job/JSON path.

The normal Go suite checks multipart forwarding, lease protection during
harvesting, telemetry, and JSON downloads after backend disconnection. To also
exercise actual Docker startup and stop eviction without loading any ML models:

```sh
go test -tags integration ./internal/api -run TestAlignerDockerLifecycle -v
```

Run that Docker check separately from other container lifecycle tests: the
supervisor's clean-slate test deliberately removes all ASS-labelled containers.
It uses a dedicated `ass-aligner-smoke` container on port 18097 and removes it
when finished. The mock image and Docker access are required.

## Client migration

Submit a vocal stem and its known lyrics. The multipart field names remain
`audio` and `params`; the response is now a job ID, not the alignment itself.

```sh
curl -s -F 'audio=@vocals.wav' \
  -F 'params={"text":"Hello world","language":"en"}' \
  http://localhost:2645/v1/aligner/jobs

curl -s http://localhost:2645/v1/jobs/JOB_ID
curl -s http://localhost:2645/v1/jobs/JOB_ID/result/alignment.json
```

Wait for `state: "succeeded"` before downloading. Failed jobs expose an error.
The artifact is JSON with `language`, `duration`, and `lines`, each containing
`text`, `start`, `end`, and `words`. Each word contains its text and start/end
seconds; unresolved timings are `null`. ASS treats it as a `metadata` artifact.

## Acceptance on the GPU host

Test short and long tracks, unresolved words, and a language switch. Verify
`/v1/backends/aligner/info` reports memory and the currently loaded language.
Run another large backend to force aligner eviction, then confirm the old JSON
still downloads from ASS and a new alignment reuses cached weights. Measure
peak VRAM and host RAM to replace the estimates. Mocked contract tests run on
the development machine; real alignment and container/GPU checks happen here.
