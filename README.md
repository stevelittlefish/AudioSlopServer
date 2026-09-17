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
