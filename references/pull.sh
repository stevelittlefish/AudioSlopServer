#!/usr/bin/env bash
#
# Clone every reference repository, or update the ones already here.
#
# Nothing in this folder is part of ASS. These are read, not imported, and they
# are gitignored — a fresh checkout starts empty and this script fills it.
#
# Two kinds of remotes live here:
#   PUBLIC_REPOS — on GitHub, reachable from anywhere.
#   LAN_REPOS    — on a private git server on the LAN. Only reachable when we
#                  are actually on that network, so we probe first and skip them
#                  (loudly, not fatally) when we are somewhere else, e.g. a
#                  coffee shop pretending to be a datacenter.

set -uo pipefail

# Each entry is either "url" or "url|dir". The optional |dir overrides the
# checkout directory, which matters when two repos share a basename — our
# ACE-Step fork and its upstream are both literally named ACE-Step-1.5, and
# without distinct dirs the second would silently pull into the first's clone.
PUBLIC_REPOS=(
  # ACE-Step 1.5 — our server fork, and the upstream it forked from.
  "git@github.com:stevelittlefish/ACE-Step-1.5.git"
  "git@github.com:ace-step/ACE-Step-1.5.git|ACE-Step-1.5-upstream"
  # Karaoke: word-level forced alignment.
  "git@github.com:stevelittlefish/forced-aligner.git"
  # Stable Audio 3 — our dockerised (probably stale) wrapper, and upstream.
  "git@github.com:stevelittlefish/stable-audio-3-docker.git"
  "git@github.com:Stability-AI/stable-audio-3.git"
  # YuE 2 — music generation.
  "git@github.com:multimodal-art-projection/YuE.git"
  # DEMUCS stem-separation server.
  "git@github.com:stevelittlefish/stem-separator.git"
  # Not a backend — a pattern reference. Its GoReleaser + GitHub Actions setup is
  # the model we copy for building/versioning/publishing our own images.
  "git@github.com:stevelittlefish/llm_proxy.git"
)

# host:port of the private LAN git server. If we can't open a socket to this,
# there is no point trying to clone anything from it.
LAN_HOST="seaslug.io"
LAN_PORT="2222"

LAN_REPOS=(
  # SlopBC — the client that talks to this server. The API contract lives on
  # both sides of this fence, so it's the reference that matters most.
  "ssh://git@seaslug.io:2222/steve/SlopBC.git"
  # the_sing_thing — karaoke system.
  "ssh://git@seaslug.io:2222/steve/the_sing_thing.git"
)

cd "$(dirname "$0")"

# Can we reach the LAN git server? Open a TCP socket with a short timeout so a
# missing LAN costs us a couple of seconds, not a hung SSH handshake.
lan_reachable() {
  timeout 3 bash -c "exec 3<>/dev/tcp/${LAN_HOST}/${LAN_PORT}" 2>/dev/null
}

failed=()

clone_or_pull() {
  local entry="$1"
  local repo="${entry%%|*}"
  local dir
  if [[ "$entry" == *"|"* ]]; then
    dir="${entry##*|}"          # explicit override
  else
    dir="${repo##*/}"; dir="${dir%.git}"
  fi

  if [ ! -d "$dir" ]; then
    echo "==> Cloning $dir"
    git clone "$repo" "$dir" || failed+=("$dir (clone)")
    return
  fi

  # Local edits to a reference checkout are almost always accidental, but they
  # are still someone's work. Say so and move on rather than failing the run or
  # trying to merge on their behalf.
  if [ -n "$(git -C "$dir" status --porcelain)" ]; then
    echo "==> Skipping $dir — working tree is dirty"
    failed+=("$dir (dirty, not pulled)")
    return
  fi

  echo "==> Pulling $dir"
  git -C "$dir" pull --ff-only || failed+=("$dir (pull)")
}

for repo in "${PUBLIC_REPOS[@]}"; do
  clone_or_pull "$repo"
done

if [ ${#LAN_REPOS[@]} -gt 0 ]; then
  if lan_reachable; then
    echo "==> LAN server ${LAN_HOST}:${LAN_PORT} is reachable — pulling private repos"
    for repo in "${LAN_REPOS[@]}"; do
      clone_or_pull "$repo"
    done
  else
    # Not on the LAN. This is expected and fine — the private repos just aren't
    # available right now. Don't count it as a failure; nobody wants a red X for
    # being on the wrong Wi-Fi.
    echo "==> LAN server ${LAN_HOST}:${LAN_PORT} unreachable — skipping ${#LAN_REPOS[@]} private repo(s)"
  fi
fi

echo
total=$(( ${#PUBLIC_REPOS[@]} + ${#LAN_REPOS[@]} ))
if [ ${#failed[@]} -eq 0 ]; then
  echo "Reference repositories are up to date (of $total known)."
else
  # One unreachable remote should not hide the fact that the others worked, so
  # problems are collected and reported here rather than aborting the loop.
  echo "Finished with ${#failed[@]} problem(s):"
  printf '  - %s\n' "${failed[@]}"
  exit 1
fi
