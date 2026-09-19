# Child-service reference checkouts

These are reference checkouts of the services that Audio Slop Server (ASS)
orchestrates or integrates with. **They are not part of this codebase.** Each
service is maintained in its own repository; these copies are only for reading
and comparing backend APIs and behavior. Do not import or vendor them into ASS.
ASS runs backend Docker images, not these local checkouts.

All checkout folders and their contents are **gitignored**. Only this README,
`pull.sh`, `.gitignore`, and the Go module boundary are versioned. `go.mod` keeps
cloned Go packages out of ASS's build and tests; Docker also excludes this tree.

From the project root:

```sh
./child_services/pull.sh  # clone missing services or pull clean checkouts
```

The script skips dirty checkouts and uses fast-forward-only pulls. Add services
to `PUBLIC_REPOS` in `pull.sh`; the ignore rules already cover new folders.
This fetches source for reference; `./pull-services.sh` fetches runtime images.

| Repository | Role |
|---|---|
| [ACE-Step-1.5-inference-server](https://github.com/stevelittlefish/ACE-Step-1.5-inference-server) | ACE-Step music generation server fork. |
| [forced-aligner](https://github.com/stevelittlefish/forced-aligner) | Word-level alignment for karaoke lyrics. |
| [stable-audio-3-docker](https://github.com/stevelittlefish/stable-audio-3-docker) | Stable Audio 3 service wrapper. |
| [YuE-inference-server](https://github.com/stevelittlefish/YuE-inference-server) | YuE music generation server; checked out as `yue-inference-server`. |
| [stem-separator](https://github.com/stevelittlefish/stem-separator) | DEMUCS stem separation server. |

Upstream implementations, clients, and other examples belong in
[`../references/`](../references/README.md).
