# CLAUDE.md — ASS (Audio Slop Server)

Guidance for working on this project.

## What it is

ASS is a single-GPU orchestration server for multiple heavyweight audio AI
services. It loads and evicts models on demand — like Ollama does for LLMs —
so that many disparate services can share **one** GPU instead of each
monopolizing its own.

## Core concept

- **One GPU, many models.** Only one (or a small set that fits) is resident in
  VRAM at a time.
- **Swap models in and out on demand**, driven by incoming requests.
- Target services include: DEMUCS (separation), Whisper (transcription),
  Stable Audio 3, YuE 2, ACE-Step 1.5 XL (generation), and more over time.

## Working agreement

- **Spec phase — no coding** until the user explicitly says to start building.
- Nuggets from the user get transcribed into README.md (product-facing) or
  this file (technical/architecture), whichever fits.
- **Commit straight to `main`.** For every dev task, commit directly to `main`
  unless told otherwise. No branches, no PRs — those are for boring corporate
  jobs and people who give a shit about the code. We're generating Slop; the
  project itself is Slop. Slop generating Slop.
- When committing, proudly announce: **"Slopping it straight to main!"**
- **Always `git push` after committing.** The first rule of Slop: Slop is for
  the masses, and the masses can't consume it while it's on our hard drive.
  Commit to `main`, then push it straight out.

## The Great Philosophy of Software Languages

1. **No JavaScript on the server. Ever.** We are not failed front-end
   engineers — we are failed *back-end* engineers. That's why we make the Slop!
   Avoid Node at all costs in committed server code.
   - **Exception:** JavaScript *is* meant for the front-end, so it's fine on the
     client. It's also acceptable to use Node/JS tooling to validate or build
     front-end code — just keep it out of the committed server code.

2. **Python is tolerated, not embraced.** Many of these AI tools are written in
   Python. This is unfortunate — it drags in the whole miserable circus of
   virtualenvs, requirements.txt, pyenv, poetry, conda, uv, and a thousand other
   stupid tools that exist purely to avoid installing packages in the system
   pip. We may have to commit *some* Python to the codebase, and that's
   accepted: Python is at least better than JavaScript. But if we can avoid it,
   we should.

3. **Go is our language of choice.** If it were up to us, the whole thing would
   be written in Go. Go is great because you just type `go run .` and it just
   works. We don't know how. We don't care. We just get a cool binary we can
   run. Go would be the language for everything here — if only the AI bastards
   hadn't all settled on Python as The One True Language™.
   - **Practical consequence:** default to Go for anything we control (the
     server, orchestration, glue, tooling). Fall back to Python only where the
     AI tooling forces our hand (see rule 2).

4. **No fancy-pants front-end frameworks.** React and all the other frameworks
   are hated. *If* we ever have a web interface (actual pages for humans):
   - Plain JavaScript only, where necessary.
   - Separate URLs per page, with server-generated HTML where possible.
   - No SPA, no build-step framework nonsense.

   That said, ASS will probably be **100% API** with no human-facing pages at
   all, so this rule may never come up. It's stated for the record because the
   frameworks are hated.

5. **No Object-Oriented Programming.** OOP is ideological nonsense invented by
   failed programmers with too much time on their hands. Write plain functions
   over plain data. No sprawling class hierarchies, no inheritance towers, no
   design-pattern cosplay. (Go makes this easy — lean on functions, structs, and
   composition, not ceremony.)

## Objectives & Coding Conventions

1. **ASCII art on startup.** When the app starts, it must print **ASS** in big
   ASCII-art letters to the log.
2. **Sarcasm required.** Both commit messages and code comments should include
   some sarcasm and witty remarks. Dry humor over dry documentation.
3. **Consistent API across sub-services.** Aim for a fairly consistent API
   shape across all the different audio sub-services, so callers don't have to
   relearn everything per service. Not a hard rule — if a service genuinely
   needs something different, exceptions are allowed.
4. **Config in TOML, not environment variables.** No environment variables
   except where absolutely necessary (i.e. something genuinely outside our
   control demands one). All configuration lives in TOML files. A dependency for
   TOML parsing is acceptable.
5. **Dependencies are expensive.** In general, treat every dependency as a cost
   to be justified. Prefer the standard library and a little of our own code
   over pulling something in. Some deps are worth it (TOML, sqlite) — but the
   default answer is "do we really need it?"
6. **Configurable memory footprint.** ASS must run on a 128GB server *and* on
   some poor peasant's 16GB laptop. Memory strategy (which services stay resident
   in RAM vs. get fully unloaded) is configured per service in TOML, with sane
   limits so we never assume the big-server case.
7. **Never vendor third-party source into this repo.** The boundary with every
   backend is HTTP, across a process/container line. We never copy their code in
   and never import it. This keeps ASS clear of copyleft (GPL etc.): running a
   GPL'd backend as a separate program we talk to over a socket is mere
   aggregation, not a derivative work, so ASS stays permissively licensed (MIT).
   Corollary: keep Go dependencies to permissive licenses (MIT/BSD/Apache) —
   those *are* linked into our binary. Need a backend's logic? Reimplement it or
   wrap it behind a service; don't paste it in.

## Architecture notes

_(TBD — to be filled in as the spec develops.)_
