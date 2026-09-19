# Measurements: hardware & memory footprints

Real numbers from real boxes, because the whole budgeting story (VRAM budget,
per-parked context tax, "don't evict the small ones") is guesswork until we
have them. This is where they live. Add rows as we measure more models and more
machines — this file is the input to the eventual concrete budget defaults.

## Image layers (why pulling backends is many × ~4.5GB)

Every backend image is three big strata, each a single Docker layer: the **CUDA
base image** (`nvidia/cuda:*-runtime`, ~2.7GB; `-devel` ~5–6GB), the **torch
layer** (the cuXYZ wheels bundle their *own* copy of the CUDA libs — cuDNN,
cuBLAS, NCCL — so `pip install torch` alone is ~4.5GB), and the **rest of the
deps**. That's the "several 4.5GB steps."

Docker layers are content-addressed, so two images share a layer on pull only
when it's byte-identical. The base image layers **are** shared when the `FROM`
tag matches exactly (they're the same registry layers). So we pin the backends
that can agree to one base tag:

| backend | base | torch | shares base? |
|---|---|---|---|
| stem-separator | `cuda:12.8.1-runtime` | 2.10.0 / cu128 | ✅ |
| ACE-Step | `cuda:12.8.1-runtime` | 2.10.0 / cu128 | ✅ |
| YuE | `cuda:12.8.1-runtime` | 2.10.0 / cu128 | ✅ |
| stable-audio | `cuda:12.6.3-devel` | 2.7.1 / cu126 | ❌ outlier |

**stable-audio can't join** without risk: its flash-attn is a *prebuilt* wheel
locked to `cu126torch2.7`, and its whole `uv.lock` is pinned to torch 2.7.1 —
moving it means re-sourcing that wheel for torch 2.10 and re-resolving the lock
(which ripples through pytorch-lightning et al.). Left as-is on purpose.

**What this fixes and what it doesn't.** Unifying the base tag makes the CUDA
base layer one shared pull across the three (was three different bases). It does
**not** dedupe the ~4.5GB *torch* layer — that's built locally on top with a
per-image instruction, so each image still carries its own. To collapse the
torch layer too we'd need a shared base image (`FROM ass-cuda-torch:12.8.1-2.10`
that already contains torch), which all three then build on. That's a bigger
change — a new published image + cross-repo build coupling — so it's a separate
call, not done here.

## Servers

| Host | GPU | VRAM | System RAM | Notes |
|---|---|---|---|---|
| `ai.lemon.com` | **4× RTX 3090** (24GB each) | 24GB/card | 128GB | **The test & deployment target**, where ASS runs. ASS uses **GPU 0** (`device = 0`). **The card is NOT single-tenant:** GPU 0 also carries **wyoming-whisper** (~2.1GB, a Home Assistant faster-whisper backend — external, not ASS-managed, holds VRAM permanently); **GPU 1 is ComfyUI's and must be left empty** (it wants the whole card on demand — do NOT move ASS there despite it looking free); GPUs 2–3 run vLLM (~21GB each). So on GPU 0 the usable VRAM budget is 24GB **minus** wyoming-whisper's ~2.1GB minus driver overhead — see the GPU-occupancy snapshot below. |
| `ai2.lemon.com` | RTX 4080 (eGPU) | 16GB | **16GB** | *Not* our test box — just where the demucs numbers below happened to get measured. A **mini PC with an eGPU over OCuLink**: laptop-class host (16GB soldered) + desktop-class card. The canonical "peasant with a fancy hat": memory-constrained, compute-rich. Here **system RAM binds before VRAM**, so favour `evict = "stop"` + `idle_ttl` reclaim over parking — reloads are cheap on this card *anyway*. OCuLink is PCIe 4.0 x4 (~8 GB/s): a real PCIe link, ~2× Thunderbolt, but ~4× less than a directly-attached x16 slot — so the park↔unpark CPU↔VRAM copy is somewhat slower than on a desktop, though far from the Thunderbolt penalty. The ~2–5s park-restore assumption is a little optimistic here, not wildly. |

### GPU occupancy on `ai.lemon.com` (measured 2026-09-18, ASS not running)

`nvidia-smi` with none of ASS's backends up, so this is the *external* load ASS
must budget around. The four cards are **not** interchangeable:

| GPU | Used (idle) | Who | ASS may use it? |
|---|---|---|---|
| **0** | 2110 MiB | **wyoming-whisper**, ~2100 MiB, permanent (PID `/app/bin/python3`) | **Yes — this is ASS's card** (`device = 0`) |
| 1 | 266 MiB | **ComfyUI** — loads/unloads on demand and **must have the card clear when it fires** | **No. Leave it empty.** |
| 2 | 21346 MiB | vLLM worker TP0 | No |
| 3 | 22793 MiB | vLLM workers TP1 (21336) + a 1442 MiB python | No |

**GPU 1 looks nearly free (266 MiB) and it is a trap.** It's ComfyUI's card;
ComfyUI expects the whole card available when a workflow runs, so ASS must never
land there — put ASS on GPU 1 and the next big ComfyUI job OOMs. `device = 0` is
deliberate, not just "the first card".

**Budget confirmed from this snapshot.** GPU 0 idles at 2110 MiB (the ~2100 MiB
whisper process + ~10 MiB driver — the 3090 has almost none of the ~0.39GB the
4080 shows below). So usable ≈ 24576 − 2100 − 10 ≈ **22466 MiB**, and
`vram_budget_mb = 21500` sits ~966 MiB under that — the right amount of headroom,
because faster-whisper grows with concurrent transcriptions and torch reserves
above what it allocates. **21500 stands, now measured rather than assumed.**

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

### All four parked at once — the real per-backend context tax (2026-09-18)

`nvidia-smi` with all four ASS backends **parked** on GPU 0 (weights to CPU, only
the CUDA context + workspaces left on the card). PIDs mapped to containers via
`docker inspect -f '{{.State.Pid}}'`. This is the number `vram_parked_mb` charges
per parked backend against the budget:

| backend | parked VRAM | process | notes |
|---|---|---|---|
| demucs | **354 MiB** | system `python3` | matches the single-backend measurement above |
| ACE-Step | **616 MiB** | `/app/.venv/bin/python` | **small despite the XL DiT** — `OFFLOAD_TO_CPU` already keeps most weights off the card, so park leaves little behind. Predicted 0.5–2GB; landed at the bottom. |
| YuE | **1042 MiB** | system `python3` | the 3B model's context + kernels |
| stable-audio | **1948 MiB** | `/app/.venv/bin/python` | **the heaviest**, ~3× ACE-Step — flash-attn + larger persistent workspaces. Was the worst-estimated in config (guessed 800). |

**Total ASS parked cost ≈ 3960 MiB** to keep all four warm — real and worth
budgeting for. Plus wyoming-whisper's 2100 → GPU 0 idled at 6091 MiB with nothing
pinned. `ass.toml` now sets each `vram_parked_mb` from these (measured + headroom):
demucs 400, ACE-Step 700, YuE 1100, stable-audio 2000.

**Two surprises worth remembering:** the park tax does **not** track model size —
ACE-Step (biggest model) parks smallest, stable-audio (medium) parks largest.
It's about how much CUDA workspace the process pins, not the weights (those are on
the CPU). And the old per-service *estimates* were badly off in both directions,
which is the whole argument for measuring.

**Note — `/v1/info` VRAM telemetry does NOT capture this.** The parked context tax
is CUDA-driver-level (context + cuBLAS/cuDNN/flash-attn workspaces); torch's
`memory_allocated`/`reserved` — what `/v1/info` and the admin VRAM table report —
read ~0 for a parked process because there are no live tensors on the card. So the
admin table gives you inference **peaks** (pinned), and per-process `nvidia-smi`
stays the tool for **parked** taxes. Different memory layers, different tools.

### Inference peaks (pinned) — from `/v1/info` telemetry (2026-09-18)

The first real pinned peaks, read off the admin VRAM table (ASS records each
backend's `peak_mb` = torch `max_memory_allocated` after every job). This is the
number `vram_pinned_mb` must cover, and **two of the four guesses were dangerously
low**:

| backend | measured peak | old guess | new `vram_pinned_mb` | verdict |
|---|---|---|---|---|
| demucs | 554 MiB | 2000 | 1000 | over-budgeted; trimmed |
| stable-audio | 5388 MiB | 6000 | 6500 | was at 90% — nudged up |
| YuE | 9486 MiB | 8000 | 10500 | **19% too low** |
| ACE-Step | 13850 MiB | 10000 | 15000 | **39% too low — would overcommit** |

Left as guesses, ASS would have pinned ACE-Step believing it costs 10GB while it
actually peaks at ~13.9GB — on a card already carrying whisper (2.1GB) and other
parked backends, that's a real OOM. This is the entire point of the budget being
a measured, per-service knob.

**`reserved` lies after a job; `peak` doesn't.** In the telemetry YuE showed
`reserved` 688 MiB but `peak` 9486 MiB — because ASS reads `/v1/info` *after* the
job and YuE has offloaded its weights to CPU by then, so the live reservation is
near-nothing while the high-water mark still records the real inference peak.
Always budget on `peak_mb`, never the post-job `reserved`/`allocated`.

Caveat: these are 1–3 samples each. Peaks grow with longer audio, more steps and
bigger batches, so treat them as a floor and let the telemetry keep accumulating.

### The aligner is different: VRAM scales with audio length

Every backend above has a roughly **fixed** pinned cost — a generator peaks about
the same whether the clip is 30s or 5min. **The forced aligner does not.** wav2vec2
CTC alignment holds the whole track's activations on the card at once, so its VRAM
peak **grows with audio duration**:

- A typical **~4 minute** song is comfortably under budget.
- A **20 minute** track pushes the aligner **over 16 GB**.

So `vram_pinned_mb` for the aligner is a **deliberate compromise, not a safe
ceiling.** Setting it to the 20-minute worst case (>16GB) would wastefully reserve
the whole card for the common 4-minute job and block co-tenants for no reason.
Setting it to the 4-minute cost risks an OOM on the rare long track. The current
**14000 MiB** is a middle guess: generous for normal songs, still short of the
longest. Options if long tracks become common: raise the reservation and accept
the waste, chunk long audio in the backend, or fail-fast over a length threshold
rather than OOM mid-align. Recorded here so nobody "fixes" the reservation to the
max and wonders why the card is always full.

**That spike is transient — the aligner frees it after every job.** The peak
above is the *inference* peak, not steady state: `align()`'s `finally` drops the
audio/alignment tensors and calls `empty_cache()`, and the image sets
`PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True` so the fragmented multi-GB
reserved pool is actually returned to the driver instead of sitting there until
the next song reuses it (which is what an earlier build did — the card looked
stuck at the long-song peak between jobs). Only the resident wav2vec2 weights
stay on the card once a job completes, so a long track no longer taxes the
co-tenants after it finishes — only while it's actually aligning.

**Parked cost still has to be measured — and that needs the park build deployed
first.** Until the park-capable forced-aligner image is released and running on
`ai.lemon.com`, there is nothing to `nvidia-smi`: a stopped backend parks nothing.
So the aligner's `vram_parked_mb` stays an **estimate (500 MiB)** in `ass.toml`,
and the measurement is blocked on the deploy, not on us. The parked tax should be
small and length-independent (a parked process holds only the CUDA context, not the
job's activations — same story as the generators above), but confirm it once the
image is on the box. Chase it via the per-process `nvidia-smi` across an
unpark→park cycle, exactly as the four backends above were measured.

### Lazy VRAM load (nvtop-confirmed) — FIXED for demucs (2026-09-18)

**Originally:** demucs used **zero VRAM until its first request** — the container
started and built the model object, but demucs deferred the weight load onto CUDA
until the first `separate` call. On nvtop: VRAM flat at 0 while idle, stepping up
to a resident floor on the first job.

**Now (stem-separator fork):** `make_separator` forces `separator.model.to(device)`
at startup, so demucs holds its resident VRAM the moment it's up — not on first
job. This was changed because lazy load defeated ASS's Load/warm button (a
"loaded" backend still cost 0 until a job) and made a freshly pinned backend's
budget cost fictional until first use. **Takes effect once the rebuilt image is
deployed.** The other backends may still lazy-load until each is checked the same
way — if a warmed backend shows ~0 VRAM in `nvidia-smi`, it's still lazy.

Consequence for the arbiter/budget: with eager load, a freshly-pinned demucs
costs its real resident VRAM immediately, so the budget is honest from container
start rather than only *post-first-use*.

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

## ACE-Step park analysis (predicted — awaiting box measurement)

`ass.toml` now sets `acestep.evict = "park"`. The reasoning, and the numbers to
confirm on `ai.lemon.com`:

**Why ACE-Step is the best park candidate, not the worst.** The original worry was
"the XL DiT is huge, parking it is expensive." But ACE-Step runs with
`ACESTEP_OFFLOAD_TO_CPU=true` + `ACESTEP_LM_OFFLOAD_TO_CPU=true`, so between renders
the VAE, Qwen text-encoder and 1.7B LM planner **already live in CPU RAM** — only
the DiT (~8–9GB, 4B XL-turbo bf16) sits on the card. The fork's `/park` moves
`model`(DiT)+`vae`+`text_encoder` to CPU and `empty_cache()`s; the LM isn't even in
that list because LM-offload already keeps it off the GPU. So **park has exactly one
big thing to move: the DiT.** One ~9GB Host↔Device copy.

**Predicted numbers (to verify):**

| Metric | Prediction | Basis |
|---|---|---|
| Unpark latency | **~0.5–1.5s** | one ~9GB DiT copy over the 3090's x16 PCIe (gen4, ~13GB/s unpinned → 20+ pinned). SA3 territory. |
| Cold start avoided (the payoff) | **~10–60s** | `stop` = kill container → reload DiT from disk/page-cache + re-init CUDA + re-init LM. Park buys all of this back for a ~1s cost. |
| Parked RAM | **~14GB** | DiT ~9 + LM 1.7B ~3.5 + text-enc ~1.2 + VAE ~0.3. Trivial on 128GB. |
| Parked VRAM (context tax) | **~0.5–2GB (UNCONFIRMED)** | bigger than demucs's 354 MiB — ACE-Step's process holds far more torch/kernel/workspace state. The one number that actually needs measuring. |

**Does it fit the shared GPU 0?** Usable ≈ 24 − 2.1 (wyoming-whisper) − 0.4 (driver)
≈ **21.5GB**. ACE-Step pinned (~9GB) + SA3 parked context (~0.5–1GB) + demucs parked
context (~0.35GB) ≈ 10–11GB. Comfortably under. **VRAM is not the constraint; the
128GB RAM certainly isn't.** The only way this doesn't pay off is if the measured
context tax is surprisingly huge (many GB) — unlikely, but that's what the
measurement is for.

**Measure on the box (fill these in):** run one `generate` job first (lazy load —
like demucs, the DiT likely isn't on CUDA until the first job), then isolate the
ACE-Step pid and read process VRAM across a park cycle, and time the unpark:

```sh
# ACE-Step's process VRAM, resident (post-first-job) vs parked:
nvidia-smi --query-compute-apps=pid,used_memory,process_name --format=csv,noheader | grep -i ace
curl -sf -X POST http://localhost:2766/park   # then re-read the line above
# Unpark latency (wall clock of the fast path):
time curl -sf -X POST http://localhost:2766/unpark
```

resident − parked = the DiT freed; the residual = ACE-Step's real `context_tax_mb`.
Record the row in "Per-model VRAM footprint" above and, if the context tax differs
from demucs's 400, note that per-backend budgeting will eventually need per-service
tax values (today `gpu.context_tax_mb` is one global number — see the deferred
VRAM-budget item in TODO.md).

## Automated sampling (min/max in one shot)

Reading `nvidia-smi` by hand catches the resident floor but almost never the
inference peak — the peak lives for a few seconds mid-job. `scripts/measure-vram.sh`
samples one process at ~5Hz in the background and remembers the extremes, so you
get both:

```sh
# On the GPU host (or wrap the whole thing in ssh):
scripts/measure-vram.sh acestep            # match the backend by process-name substring
scripts/measure-vram.sh --port 2766 acestep # …and auto-park at the end for the context tax
scripts/measure-vram.sh --pid 12345        # or pin an exact pid
```

Start it, run one or more jobs against the service (test page or curl), then
Ctrl-C. It prints **min** (resident/parked floor, ignoring the pre-load zeros of
a lazy loader) and **max** (the inference peak the VRAM budget must fit); with
`--port` it then POSTs `/park` and reports the parked floor and the weights
freed. Record the row above.

These map straight onto the arbiter's VRAM budget knobs in `ass.toml`:

- **max → `services.<svc>.vram_pinned_mb`** — the peak the budget must fit. This
  is the number to raise if a service OOMs.
- **parked floor → `services.<svc>.vram_parked_mb`** — the context tax a parked
  backend keeps on the card (falls back to `gpu.context_tax_mb` if unset).

The arbiter keeps `Σ pinned vram_pinned_mb + Σ parked vram_parked_mb` under
`gpu.vram_budget_mb`, evicting LRU residents until a newcomer fits. It trusts
these declared numbers (a soft budget), so an under-declared peak can still OOM —
the fix is to bump `vram_pinned_mb`.

## Automatic recording (ASS collects this itself now)

Beyond the one-off `measure-vram.sh` runs, ASS records real VRAM continuously:
every backend reports its GPU memory in `/v1/info` (`allocated_mb`, `reserved_mb`,
`peak_mb` — the last is torch's high-water mark, so it's the inference peak), and
ASS reads it after each job (model still resident) into the `vram_samples` table.

- **`GET /v1/vram`** returns the per-service rollup — `max_peak_mb`,
  `max_reserved_mb`, sample count — alongside each service's configured
  `vram_pinned_mb`/`vram_parked_mb`, so you can eyeball measured-vs-budget.
- This is the calibration loop for the budget: run real traffic, then set each
  service's `vram_pinned_mb` from its observed `max_peak_mb` (+ headroom). No GPU
  access on ASS, no polling — one HTTP read per job. `cuda:false` backends (the
  dev box) are skipped, so the table holds only real numbers.

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

## Forced aligner — initial estimates (not measured)

The new `aligner` service starts with `vram_pinned_mb = 12000` and
`ram_reserve_mb = 6000`, in MiB. Stop eviction means no parked model or parked
VRAM reservation. Only one language model is held at a time; changing language
unloads the previous model before loading another.

The VRAM estimate includes headroom above a historical source-code note of
roughly 9.9 GB reserved after a ten-minute track. That is not a measurement of
this ASS deployment, nor a limit enforced on individual jobs. Input duration and
language affect the peak. The RAM value is an initial allowance for the model,
audio and alignment scratch, also unmeasured.

On the GPU server, measure short and long songs, a language switch, warm and
cold starts, and another backend forcing eviction. Record allocated/reserved/
peak VRAM from `/v1/backends/aligner/info`, total driver VRAM and process RAM.
Verify cached weights survive eviction and ASS still serves the old
`alignment.json` after the container is removed. Replace these estimates with
measured reservations and headroom before relying on packing near the GPU limit.
