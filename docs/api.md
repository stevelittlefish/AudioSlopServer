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

## Request formats per service

Every service uses the **same** submit/poll/download loop above. Only the **body
of the submit** differs, and this section spells out each one exactly. Which
backends your instance actually has is configured in TOML, so the list may vary.

> 🐍 **Don't want to read? Let the app write the request for you.** With ASS
> running, open **`http://localhost:2645/test/<service>`** (e.g. `/test/yue`),
> fill in the form, and click the **JSON** tab — that's the *exact* body ASS
> sends. Copy it straight into a `curl`. The forms are the source of truth these
> docs are generated from; when in doubt, trust the JSON tab.

### How bodies are encoded

There are two wire formats. A service uses one or the other (noted per service):

- **JSON** — `Content-Type: application/json`, body is the params object. Used by
  services that take no file (text-to-audio generators).
- **Multipart** — `multipart/form-data` with **one JSON part carrying all the
  params**, plus a part per uploaded file. The JSON part is named `params` for
  most services, `param_obj` for ACE-Step (noted below). Used by anything that
  takes an audio file.

All examples assume ASS on `localhost:2645`. Fields not listed are rejected by
the backends (extras → `422`), so send only what's documented.

---

### `demucs` — source separation (`separate`)

**Multipart** (`params` JSON part + `audio` file). Splits a mix into stems.

| Field | Type | Notes |
|---|---|---|
| `audio` | file | **Required.** The mixed track (multipart file part, not in the JSON). |
| `mode` | string | `two-stem` (vocals + instrumental) or `four-stem` (vocals/drums/bass/other). |
| `file_format` | string | `wav` \| `flac` \| `mp3`. |
| `shifts` | int | Test-time augmentation passes (default 1). Higher = slower, slightly better. |
| `overlap` | float | Chunk overlap, 0–0.99 (default 0.25). |

```sh
curl -X POST http://localhost:2645/v1/demucs/jobs \
  -F 'params={"mode":"two-stem","file_format":"wav"}' \
  -F 'audio=@song.wav'
```

Artifacts: `vocals`, `no_vocals` (two-stem); `vocals`, `drums`, `bass`, `other`
(four-stem).

---

### `aligner` — forced alignment (`align`)

**Multipart** (`params` JSON part + `audio` file). Lines up sung/spoken words to
timestamps.

| Field | Type | Notes |
|---|---|---|
| `audio` | file | **Required.** The audio to align. |
| `text` | string | **Required.** The words, keeping original line breaks. |
| `language` | string | ISO code, e.g. `en`. Optional (auto if omitted). |

```sh
curl -X POST http://localhost:2645/v1/aligner/jobs \
  -F 'params={"text":"Morning light on an empty street","language":"en"}' \
  -F 'audio=@vocals.wav'
```

Artifact: `alignment.json` (per-word timings). Standalone clients can also call
the backend's synchronous `/align` directly — see [aligner.md](aligner.md).

---

### `yue` — music generation, YuE2 (`generate`)

**JSON.** A style description + lyrics become an ABC score, then a full song.

| Field | Type | Notes |
|---|---|---|
| `lyrics` | string | **Required.** Segment with tags like `[verse]`, `[chorus]`. |
| `style` | string | Genre / instruments / mood / tempo, natural language. |
| `stage` | string | `audio` (full song) or `plan` (ABC score only — fast). |
| `cot` | string | Chain-of-thought planning: `full` \| `melody` \| `off`. Omit for server default. |
| `file_format` | string | `flac` \| `wav`. |
| `seed` | int | Omit for a random song each run. |
| `cfg_scale` | float | Guidance scale (0–20). Omit for default. |
| `abc` | string | Provide an ABC score directly (advanced). |
| `abc_sampling`, `semantic_sampling` | object | Advanced sampler overrides (JSON objects). |

```sh
curl -X POST http://localhost:2645/v1/yue/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "style":  "acoustic pop, warm piano, soft female vocal, 90 bpm",
    "lyrics": "[verse]\nMorning light on an empty street\n[chorus]\nBut I will carry on",
    "stage":  "audio",
    "file_format": "flac"
  }'
```

Artifacts: `audio.flac`, `score.abc`, `lyrics.txt`, plus metadata.

---

### `stableaudio` — text-to-audio, Stable Audio 3 (`generate`)

**Multipart** (`params` JSON part; a file part only for the variation/inpaint
workflows). Three workflows, selected by which fields you send.

Common params:

| Field | Type | Notes |
|---|---|---|
| `prompt` | string | The text prompt. |
| `seconds_total` | int | Clip length (default = model max). |
| `steps` | int | Diffusion steps. |
| `seed` | int | `-1` = random. |
| `cfg_scale` | float | Guidance. Omit for model default. |
| `negative_prompt` | string | What to avoid. |
| `batch_size` | int | Clips per run. |
| `file_format` | string | e.g. `wav`, `flac`. |
| `return_spectrogram` | bool | Also emit a spectrogram image. |
| `loras` | array | `[{"strength": 0.8}]` — indexes match the load order in `/v1/backends/stableaudio/info`. |

- **Text-to-audio** (no file): send the common params as the `params` part.
- **Variation** (restyle an init clip): add the init audio file + `init_noise_level` (0.01–1).
- **Inpaint** (regenerate a masked span): add the file + `inpaint_mask_starts` / `inpaint_mask_ends` (comma-separated seconds).

```sh
# text-to-audio
curl -X POST http://localhost:2645/v1/stableaudio/jobs \
  -F 'params={"prompt":"warm analog ambient pad, slow evolving, spacious","seconds_total":30,"seed":-1}'
```

Artifacts: the audio clip (+ `spectrogram` when requested).

---

### `acestep` — music generation, ACE-Step 1.5 XL (`generate`)

Task selected by **`task_type`**. `text2music` is **JSON**; the source-clip tasks
(`cover`, `repaint`, `extract`) are **multipart with the JSON part named
`param_obj`** plus a `ctx_audio` file.

Common params:

| Field | Type | Notes |
|---|---|---|
| `task_type` | string | **Required.** `text2music` \| `cover` \| `repaint` \| `extract`. |
| `prompt` | string | Caption / description. |
| `lyrics` | string | Optional; `[verse]` / `[chorus]` tags. |
| `vocal_language` | string | e.g. `en` (default). |
| `audio_format` | string | `mp3` \| `flac` \| `wav` \| `wav32` \| `opus` \| `aac`. |
| `audio_duration` | int | Seconds; omit for auto. |
| `inference_steps` | int | Default ~8 (turbo). |
| `guidance_scale` | float | Default ~7.0. |
| `seed` | int | `-1` = random (also sets `use_random_seed`). |
| `batch_size` | int | Songs per run. |
| `thinking` | bool | 5Hz LM plans the codes first. |
| `use_cot_caption` | bool | LM rewrites the caption. |

Source-clip extras — **`cover`**: `audio_cover_strength`, `cover_noise_strength`;
**`repaint`**: `repainting_start`, `repainting_end`, `repaint_mode`
(`balanced`\|`conservative`\|`aggressive`), `repaint_strength`; **`extract`**:
`track_name`, `extract_codes_only`.

```sh
# text2music (JSON)
curl -X POST http://localhost:2645/v1/acestep/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "task_type": "text2music",
    "prompt": "dreamy synth-pop, warm analog pads, female vocals, 90 bpm",
    "lyrics": "[verse]\n...",
    "audio_format": "mp3",
    "seed": -1
  }'

# cover (multipart — note the param_obj part name + ctx_audio file)
curl -X POST http://localhost:2645/v1/acestep/jobs \
  -F 'param_obj={"task_type":"cover","prompt":"lo-fi remix","audio_cover_strength":0.8}' \
  -F 'ctx_audio=@source.mp3'
```

Artifacts: the rendered audio plus its 5Hz code blueprint, lyrics, and metadata.

---

### `whisper` — transcription (`transcribe`)

**Multipart** (`params` JSON part + `audio` file). Not yet conformed/enabled on
every instance — check `GET /v1/backends` for whether it's present, and
`GET /v1/backends/whisper/info` for its options.

---

**Rule of thumb for anything not covered here:** `GET /v1/backends/{service}/info`
reports what the backend supports, and `http://localhost:2645/test/{service}`'s
JSON tab shows the exact body for it. ASS forwards your body verbatim, so those
two always reflect reality.

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
