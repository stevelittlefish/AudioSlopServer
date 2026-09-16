# Reference projects

Repositories kept here to **read**, not to depend on. They are cloned and
gitignored: nothing in this folder is part of ASS, and nothing here should ever
be imported by it.

```sh
./pull.sh      # clone anything missing, pull anything already present
```

## Public vs. LAN repositories

`pull.sh` splits its remotes into two groups:

- **Public repos** live on GitHub and are pulled unconditionally.
- **LAN repos** live on a private git server (`seaslug.io:2222`) that is only
  reachable from the local network. `pull.sh` probes the server with a short TCP
  connection first; if it can't reach it, the private repos are **skipped
  quietly** rather than hanging on an SSH handshake or failing the run. Being on
  the wrong Wi-Fi is not an error.

Adding a repository means editing `pull.sh` alone — put it in `PUBLIC_REPOS` or
`LAN_REPOS` as appropriate.

## The references

| Repo | Where | Why it's here |
|---|---|---|
| [SlopBC](ssh://git@seaslug.io:2222/steve/SlopBC.git) | LAN | **The client that talks to this server.** The API contract lives on both sides of this fence, so it's the reference that matters most. |
| [the_sing_thing](ssh://git@seaslug.io:2222/steve/the_sing_thing.git) | LAN | **Karaoke system** — the consumer of forced-alignment for synced lyrics. |
| [ACE-Step-1.5](https://github.com/stevelittlefish/ACE-Step-1.5) | GitHub | **Our server fork** of ACE-Step 1.5 (music generation). The one we actually run. |
| [ACE-Step-1.5 (upstream)](https://github.com/ace-step/ACE-Step-1.5) | GitHub | The OG ACE-Step 1.5 repo — upstream truth, for comparing against our fork. Cloned to `ACE-Step-1.5-upstream` so it doesn't collide with our fork's dir. |
| [forced-aligner](https://github.com/stevelittlefish/forced-aligner) | GitHub | Word-level forced alignment — **for karaoke** (lyrics timing). |
| [stable-audio-3-docker](https://github.com/stevelittlefish/stable-audio-3-docker) | GitHub | Our dockerised Stable Audio 3 wrapper. **Probably out of date** — trust upstream over it. |
| [stable-audio-3 (upstream)](https://github.com/Stability-AI/stable-audio-3) | GitHub | The OG Stability-AI Stable Audio 3 repo — upstream truth. |
| [YuE](https://github.com/multimodal-art-projection/YuE) | GitHub | **YuE 2** — music generation engine. |
| [stem-separator](https://github.com/stevelittlefish/stem-separator) | GitHub | **DEMUCS server** — source/stem separation. |

_More references will be added as we work out what ASS actually needs to do._
