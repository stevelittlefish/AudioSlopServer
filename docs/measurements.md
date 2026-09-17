# Measurements: hardware & memory footprints

Real numbers from real boxes, because the whole budgeting story (VRAM budget,
per-parked context tax, "don't evict the small ones") is guesswork until we
have them. This is where they live. Add rows as we measure more models and more
machines — this file is the input to the eventual concrete budget defaults.

## Servers

| Host | GPU | VRAM | System RAM | Notes |
|---|---|---|---|---|
| `ai.lemon.com` | (big box) | — | 128GB | **The test & deployment target.** The roomy server; keeps things parked forever. This is where ASS actually runs. |
| `ai2.lemon.com` | RTX 4080 | 16GB | **16GB** | *Not* our test box — just where the demucs numbers below happened to get measured. Fast, but RAM-constrained ("peasant-ish"): here **system RAM binds before VRAM.** Useful as the constrained-box reference. |

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

### What this tells us

- **Demucs is small.** Half a gig resident, under 2GB even mid-separation. On a
  16GB card this barely registers — a strong real-world case for the deferred
  "small services ride along, don't evict them" policy: many demucs-sized models
  could stay resident at once.
- **On `ai2`, VRAM is not the problem — the 16GB system RAM is.** Parking moves
  weights *into* system RAM, so on this box `memory.ram_budget_mb` is the limit
  that bites first, and the `idle_ttl` parked→stopped reclaim (hand RAM back to
  the OS) is the setting that matters here — not on the 128GB box.
- The `context_tax_mb` guess (500MB VRAM per parked/alive process) is still
  unmeasured. Measure it directly: park demucs, watch what VRAM it *keeps* held
  with weights on the CPU. That number is the real per-parked reserve.

## Method (so numbers stay comparable)

`nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits` (MiB), sampled
at: all containers down (baseline), backend up + idle (resident), and during an
active job (peak). Note the model name and any non-default knobs (shifts,
overlap) — they move the peak.
