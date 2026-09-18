#!/usr/bin/env bash
# Measure a backend's VRAM min/max by sampling per-process nvidia-smi while you
# exercise it. The card is shared (GPU 0 on ai.lemon.com carries other tenants),
# so we watch ONE process, never the whole card — see docs/measurements.md.
#
# The trick this automates: "max" only appears mid-inference and a single manual
# poll almost always misses it. So we sample fast in the background and remember
# the extremes.
#
#   scripts/measure-vram.sh acestep        # match the process by name substring
#   scripts/measure-vram.sh --pid 12345    # or pin an exact pid
#   scripts/measure-vram.sh --port 2766 acestep   # also auto-park at the end to read the floor
#
# Usage: start it, then run one or more jobs against the service (test page or
# curl). It prints a live line; Ctrl-C to stop and get the summary.
#
# min (>0)  = the resident/parked floor — the context tax an alive process holds.
# max       = peak during active inference. THIS is what the VRAM budget must fit.
# On a lazy loader VRAM reads 0 until the first job; min ignores those zeros.
set -euo pipefail

INTERVAL=0.2   # seconds between samples; fast enough to catch a generation peak
PID=""
NAME=""
PARK_PORT=""   # if set, POST /park at the end and report the parked floor too

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pid)  PID="$2"; shift 2 ;;
    --port) PARK_PORT="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) NAME="$1"; shift ;;
  esac
done

if [[ -z "$PID" && -z "$NAME" ]]; then
  echo "give a process-name substring (e.g. acestep) or --pid N" >&2; exit 2
fi

command -v nvidia-smi >/dev/null || { echo "nvidia-smi not found — run this on the GPU host (or via ssh)" >&2; exit 1; }

# One sample: the used_memory (MiB) for our process, or empty if it isn't on the
# card yet (lazy load, or parked to nothing). Matches by pid if given, else by
# the process_name column.
sample() {
  local line
  line=$(nvidia-smi --query-compute-apps=pid,used_memory,process_name \
                    --format=csv,noheader,nounits 2>/dev/null || true)
  if [[ -n "$PID" ]]; then
    awk -F', *' -v p="$PID" '$1==p {print $2}' <<<"$line" | head -n1
  else
    awk -F', *' -v n="$NAME" 'tolower($3) ~ tolower(n) {print $2}' <<<"$line" | head -n1
  fi
}

MIN="" MAX=0 N=0 LAST=""
echo "Watching ${PID:+pid $PID}${NAME:+process ~ '$NAME'} every ${INTERVAL}s. Run your jobs now; Ctrl-C for the summary."

summary() {
  echo
  echo "──────── VRAM summary (${N} samples on the card) ────────"
  if [[ "$MAX" -eq 0 ]]; then
    echo "  never saw the process use VRAM — wrong name/pid, or it never loaded onto CUDA."
  else
    echo "  min (resident/parked floor): ${MIN:-?} MiB"
    echo "  max (inference peak):        ${MAX} MiB"
  fi
  if [[ -n "$PARK_PORT" ]]; then
    echo "  parking (POST :$PARK_PORT/park) to read the context tax…"
    if curl -sf -X POST "http://localhost:${PARK_PORT}/park" >/dev/null; then
      sleep 1
      local parked; parked=$(sample)
      echo "  parked floor (context tax):  ${parked:-0} MiB"
      [[ -n "$MIN" && -n "$parked" ]] && echo "  freed by park (weights):     $(( MIN - parked )) MiB"
    else
      echo "  /park call failed (backend down, or no park endpoint)."
    fi
  fi
  echo "  → record this row in docs/measurements.md"
  exit 0
}
trap summary INT TERM

while true; do
  v=$(sample); N=$((N+1))
  if [[ -n "$v" && "$v" -gt 0 ]]; then
    [[ -z "$MIN" || "$v" -lt "$MIN" ]] && MIN="$v"
    [[ "$v" -gt "$MAX" ]] && MAX="$v"
    LAST="$v"
    printf "\r  now: %5s MiB   min: %5s   max: %5s   " "$v" "${MIN:-?}" "$MAX"
  else
    printf "\r  now:   (off card)   min: %5s   max: %5s   " "${MIN:-?}" "$MAX"
  fi
  sleep "$INTERVAL"
done
