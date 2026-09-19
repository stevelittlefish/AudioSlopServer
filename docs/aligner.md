# Forced alignment through ASS

The backend source is maintained separately in
[stevelittlefish/forced-aligner](https://github.com/stevelittlefish/forced-aligner),
checked out locally under `child_services/forced-aligner`. Its ASS preparation
replaces the synchronous `/align` API; clients should use ASS's job API.

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
