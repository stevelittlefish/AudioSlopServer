# Measurements: hardware & memory footprints

Real numbers from real boxes, because the whole budgeting story (VRAM budget,
per-parked context tax, "don't evict the small ones") is guesswork until we
have them. This is where they live. Add rows as we measure more models and more
machines — this file is the input to the eventual concrete budget defaults.

## Servers

| Host | GPU | VRAM | System RAM | Notes |
|---|---|---|---|---|
| `ai.lemon.com` | **4× RTX 3090** (24GB each) | 24GB/card | 128GB | **The test & deployment target**, where ASS runs. ASS uses **GPU 0** (`device = 0`). **The card is NOT single-tenant:** GPU 0 also carries **wyoming-whisper** (~2.1GB, a Home Assistant faster-whisper backend — external, not ASS-managed, holds VRAM permanently), and GPUs 2–3 run vLLM (~21GB each). So on GPU 0 the usable VRAM budget is 24GB **minus** wyoming-whisper's ~2.1GB minus driver overhead — see the budgeting note below. |
| `ai2.lemon.com` | RTX 4080 (eGPU) | 16GB | **16GB** | *Not* our test box — just where the demucs numbers below happened to get measured. A **mini PC with an eGPU over OCuLink**: laptop-class host (16GB soldered) + desktop-class card. The canonical "peasant with a fancy hat": memory-constrained, compute-rich. Here **system RAM binds before VRAM**, so favour `evict = "stop"` + `idle_ttl` reclaim over parking — reloads are cheap on this card *anyway*. OCuLink is PCIe 4.0 x4 (~8 GB/s): a real PCIe link, ~2× Thunderbolt, but ~4× less than a directly-attached x16 slot — so the park↔unpark CPU↔VRAM copy is somewhat slower than on a desktop, though far from the Thunderbolt penalty. The ~2–5s park-restore assumption is a little optimistic here, not wildly. |

### The VRAM baseline (~0.39GB with everything off)

With **all** Docker containers shut down, `ai2` still shows ~0.39GB VRAM used.
This is expected, not a leak: the NVIDIA driver plus any display server /
compositor / `nvidia-persistenced` holds a few hundred MB even with no CUDA
process running. Treat it as **unusable overhead** — set `gpu.vram_budget_mb`
against ~15.6GB, not the full 16GB.

## Per-model VRAM footprint

Measured as GPU memory *above the ~0.39GB baseline* unless noted. "Resident" =
model loaded, idle. "Peak" = during active inference.

| Backend | Model | Resident (idle) | Peak (active) | Measured on |
|---|---|---|---|---|
| stem-separator | `htdemucs_ft` | ~0.5GB (0.9GB total − 0.39 baseline) | ~1.6GB (peaked just under 2GB total) | `ai2`, RTX 4080 |
| stem-separator | `htdemucs_ft` | **1018 MiB** process total (see park below) | — | `ai.lemon.com`, RTX 3090 |

### Park measured — the real context tax (ai.lemon.com, RTX 3090)

Per-process `nvidia-smi` across a park/unpark cycle, isolating the demucs process
(pid, not the whole card — GPU 0 had other tenants):

| demucs process | VRAM |
|---|---|
| resident (unparked) | **1018 MiB** |
| parked (weights → CPU) | **354 MiB** |
| **freed by park (model weights)** | **664 MiB** |

So `/park` works exactly as intended: `model.to('cpu')` + `empty_cache()` drops
the process 1018 → 354 MiB, deterministically, every cycle. The **354 MiB that
remains is the real context tax** — the CUDA context + cuDNN/cuBLAS workspaces an
alive parked process holds with its weights on the CPU. `ass.toml` now sets
`context_tax_mb = 400` (measured 354 + a little headroom, since workspace size
can vary with input shape). The model itself is only ~664 MiB on the card —
demucs really is small.

### Lazy VRAM load (nvtop-confirmed)

demucs uses **zero VRAM until its first request** — the container starts, the
model object is built, but demucs defers the actual weight load onto CUDA until
the first `separate` call. On nvtop: VRAM flat at 0 while idle, steps up to a
resident floor on the first job, then stays there; GPU compute spikes per job.

Consequence for the arbiter/budget: a freshly-started ("pinned") backend costs
~0 VRAM until it actually runs a job. Budget against the *post-first-use*
resident + peak figures, not container start.

### What this tells us

- **Demucs is small.** Half a gig resident, under 2GB even mid-separation. On a
  16GB card this barely registers — a strong real-world case for the deferred
  "small services ride along, don't evict them" policy: many demucs-sized models
  could stay resident at once.
- **On `ai2`, VRAM is not the problem — the 16GB system RAM is.** Parking moves
  weights *into* system RAM, so on this box `memory.ram_budget_mb` is the limit
  that bites first, and the `idle_ttl` parked→stopped reclaim (hand RAM back to
  the OS) is the setting that matters here — not on the 128GB box.
- The `context_tax_mb` is now **measured, not guessed**: ~354 MiB for demucs on a
  3090 (see the park table above). `ass.toml` reserves 400. Other backends will
  differ — measure each the same way (park it, read its process VRAM).
- **GPU 0 on `ai.lemon.com` is shared, and ASS can't see the other tenants.**
  wyoming-whisper (~2.1GB) sits on the same card permanently and is not an ASS
  backend, so ASS will never evict it. `gpu.vram_budget_mb` must therefore be set
  to **(card VRAM − external tenants − driver overhead)**, not the full 24GB — it
  is the budget for what ASS itself may put on the card (pinned model + each
  parked backend's context tax), and it has to stay under whatever the other
  tenants leave free. This is the concrete reason the budget is a config knob and
  not just "the card size."

## Method (so numbers stay comparable)

`nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits` (MiB), sampled
at: all containers down (baseline), backend up + idle (resident), and during an
active job (peak). Note the model name and any non-default knobs (shifts,
overlap) — they move the peak.

**On a shared card, measure per-process, not per-card.** GPU 0 on `ai.lemon.com`
carries other tenants (wyoming-whisper, etc.), so total card `memory.used` can't
be attributed to one backend. Use the per-process query and pick out the
backend's pid:

```sh
nvidia-smi --query-compute-apps=pid,used_memory,process_name --format=csv,noheader
```

To measure a backend's park behaviour: read its process VRAM, POST `/park`, read
again — the drop is the model weights, the residual is that backend's real
`context_tax_mb`. (That's how the demucs 1018→354 MiB figures were taken.)

These reads can be done remotely: `ssh -o BatchMode=yes ai.lemon.com 'nvidia-smi …'`
works from a dev box, so measurement doesn't require sitting on the host.
