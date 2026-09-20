# ASS API Reference

**ASS (Audio Slop Server)** is a single-GPU orchestration server for heavyweight
audio AI backends. It loads and evicts whole dockerised models on demand — like
Ollama does for LLMs — so many services (separation, transcription, music
generation, forced alignment) share **one** GPU instead of each hogging its own.

You talk to **one** front door. ASS makes sure the right backend is resident,
proxies your job to it, harvests the results, and serves them back — even after
that backend has been evicted to make room for the next one.

- **Base URL:** `http://<host>:2645` (default port `0xA55` = "ASS" in hex)
- **Everything is async.** Generation takes minutes; you submit, then poll.
- **One envelope for every service.** Learn it once, use it for all of them.
- **No auth (yet).** Don't expose ASS to the open internet. Firewall it.

---

## The 30-second tour

```sh
# 1. Submit a job to a service (body format depends on the service — see below)
curl -X POST http://localhost:2645/v1/demucs/jobs \
     -F audio=@song.wav
# -> {"job_id":"job_abc123"}

# 2. Poll until state is "succeeded" (or "failed")
curl http://localhost:2645/v1/jobs/job_abc123
# -> {"job_id":"job_abc123","service":"demucs","state":"running","artifacts":[]}
# ... a bit later ...
# -> {"job_id":"...","state":"succeeded","artifacts":[
#      {"name":"vocals","kind":"stem","content_type":"audio/wav","bytes":5242880}, ...]}

# 3. Download the artifact(s) you want
curl -OJ http://localhost:2645/v1/jobs/job_abc123/result/vocals
```

That's the whole loop: **submit → poll → download**. The first request to a cold
service may take 10–60s while ASS starts the container; subsequent swaps between
already-warm backends are a few seconds.

---

## Job lifecycle endpoints

### `POST /v1/{service}/jobs` — submit a job

Submits work to a named service. ASS ensures that service is resident (starting
or un-parking it if needed — your request may queue behind a model swap), then
forwards your request body **verbatim** to the backend's job endpoint.

**The request body is service-specific.** ASS passes it through untouched, so you
send whatever the backend wants — typically `multipart/form-data` with an audio
file for input-audio services (DEMUCS, Whisper, aligner), or JSON with a prompt
for text-to-audio generators (Stable Audio, ACE-Step, YuE). See
[Services](#services) below and each backend's own `/v1/backends/{service}/info`.

Returns **`202 Accepted`** immediately — the work continues in the background:

```json
{ "job_id": "job_abc123" }
```

| Status | Meaning |
|---|---|
| `202` | Accepted; work started. Poll the job. |
| `404` | Unknown service. |
| `400` | Couldn't read your request body. |
| `500` | Failed to enqueue the job. |

### `GET /v1/jobs/{id}` — poll a job

```json
{
  "job_id": "job_abc123",
  "service": "demucs",
  "state": "succeeded",
  "created_at": "2026-09-20T12:00:00Z",
  "started_at": "2026-09-20T12:00:03Z",
  "finished_at": "2026-09-20T12:01:15Z",
  "artifacts": [
    { "name": "vocals",    "kind": "stem", "content_type": "audio/wav", "bytes": 5242880 },
    { "name": "no_vocals", "kind": "stem", "content_type": "audio/wav", "bytes": 5242880 }
  ]
}
```

`state` is one of **`queued`**, **`running`**, **`succeeded`**, **`failed`**. On
failure, an `error` field describes what went wrong. `artifacts` is empty until
the job succeeds and ASS has harvested the results into its own store.

Returns `404` for an unknown job id.

### `GET /v1/jobs/{id}/result` — the artifact list (or the single file)

Returns the artifact list:

```json
{ "artifacts": [ { "name": "vocals", "kind": "stem", "content_type": "audio/wav", "bytes": 5242880 }, ... ] }
```

**Convenience:** if the job produced exactly **one** artifact, this streams that
file's bytes directly instead of the list — handy for single-output generators.

### `GET /v1/jobs/{id}/result/{name}` — download one artifact

Streams one named artifact's bytes from ASS's own results store, with the right
`Content-Type` and a `Content-Disposition` filename. Available **even after the
backend has been evicted** — ASS owns the bytes, not the container.

Supports range requests (via `http.ServeContent`), so seeking/resuming works.

---

## Understanding artifacts

A job is **not** "a WAV". It yields zero-to-many **artifacts**, because DEMUCS
returns 2–4 stems and YuE returns a FLAC *plus* an ABC score *plus* lyrics — not
all of which are even audio. So the artifact is the unit:

| Field | Meaning |
|---|---|
| `name` | Unique within the job: `"vocals"`, `"audio.flac"`, `"score.abc"`. Use it in the download URL. |
| `kind` | Advisory role for clients: `audio`, `stem`, `score`, `lyrics`, `metadata`, `other`. Lets you say "give me the audio one" without hardcoding filenames. |
| `content_type` | Real MIME type — not everything is `audio/*`. |
| `bytes` | Size. |

---

## Backend introspection & status

### `GET /v1/backends/{service}/info`

Read-only passthrough of a backend's own `/v1/info` (model, device,
capabilities, e.g. Stable Audio's LoRA list). ASS holds no CUDA context, so this
is how you read what only the backend knows. **It warms the backend to answer.**
Returns the backend's JSON verbatim.

### `GET /v1/backends` — residency & VRAM snapshot

What every configured backend is doing right now:

```json
{
  "backends": [
    {
      "name": "demucs", "image": "stem-separation:local", "verb": "separate",
      "evict": "park", "residency": "pinned", "leases": 1,
      "last_used": "2026-09-20T12:01:15Z",
      "vram_mb": 1200, "vram_pinned_mb": 1200, "over_budget": false,
      "phase": "", "phase_since": ""
    }
  ]
}
```

- `residency` — `pinned` (hot in VRAM), `parked` (weights in CPU RAM, fast to
  restore), or `stopped` (container down).
- `leases` — in-flight jobs currently holding this backend (protects it from
  eviction).
- `vram_mb` — what it costs the card right now.
- `over_budget` — its single reservation is larger than the whole VRAM budget;
  ASS still loads it, but a heavy job here may OOM.
- `phase` / `phase_since` — what's happening mid-swap (e.g. cold-starting), for
  progress display.

### `GET /v1/vram` — measured VRAM rollup

Per-service high-water marks from the samples ASS records after every job, with
the configured budget alongside — so you can spot a service whose real peak is
creeping toward (or past) its reservation.

### `GET /health`

Liveness check: `{ "status": "ok", "service": "ASS" }`.

---

## Operator controls

These drive a backend's residency by hand. They're **real "free/kill the GPU"
buttons with no auth**, so they're gated behind the `[web]` config (on by
default); a box that wants API-only can turn them off. All route through the
arbiter, so leases still protect in-flight jobs (you'll get `409 Conflict` if a
backend is busy).

| Endpoint | Effect |
|---|---|
| `POST /v1/backends/{service}/load` | Make the backend resident (pinned). |
| `POST /v1/backends/{service}/park` | Move weights to CPU RAM, freeing VRAM. |
| `POST /v1/backends/{service}/unpark` | Restore parked weights to the GPU. |
| `POST /v1/backends/{service}/stop` | Stop the container entirely. |
| `POST /v1/backends/unload-all` | Free the GPU: evict every backend. |
| `POST /v1/backends/preload-all` | Cold-start & park every parkable backend (slow). |

Each single-backend op returns `{ "status": "ok", "service": "...", "residency": "..." }`.

---

## Services

Which backends exist is **configured in TOML**, not baked in — so your instance's
list may differ. Typical services and their verbs:

| Service | Verb | Does | Typical input | Typical artifacts |
|---|---|---|---|---|
| `demucs` | `separate` | Source separation | audio file | `vocals`, `no_vocals` (+ `drums`, `bass`) |
| `whisper` | `transcribe` | Transcription | audio file | transcript (text/JSON) |
| `stableaudio` | `generate` | Text-to-audio (Stable Audio 3) | JSON prompt | `audio.wav` (+ spectrogram) |
| `acestep` | `generate` | Music generation (ACE-Step) | JSON prompt | `audio` |
| `yue` | `generate` | Music generation (YuE 2) | JSON prompt | FLAC + `score.abc` + lyrics |
| `aligner` | `align` | Forced alignment | audio + text | `alignment.json` |

The exact request body for each is defined by that backend — query
`GET /v1/backends/{service}/info` for its capabilities, or check the backend's
own docs. ASS's job is the envelope, not the per-service knobs.

---

## Errors

Errors are JSON: `{ "error": "human-readable message" }`. Common codes:

| Code | When |
|---|---|
| `400` | Bad request body, or an operation the backend doesn't support (e.g. park). |
| `404` | Unknown service, job, or artifact. |
| `409` | Backend busy / wrong residency for the requested op (leases held, GPU busy). |
| `502` | A backend returned an error to ASS. |
| `500` | Something broke inside ASS. |

---

## Notes for API consumers

- **Poll politely.** Jobs run for seconds to minutes. A 1–2s poll interval is
  plenty; there's no need to hammer it. (An SSE stream is planned; for now it's
  polling.)
- **Cold vs. warm.** The first job to a stopped service pays a container
  cold-start (10–60s). After that, swaps between parked backends are a few
  seconds. If latency matters, `preload-all` at startup.
- **Results outlive the model.** Once a job succeeds, its artifacts are ASS's —
  download them whenever; the backend that made them can be long gone.
- **One GPU, one swap at a time.** Requests for a *different* backend queue
  behind the model swap. Requests for the *resident* backend run straight
  through against its own queue.
```
