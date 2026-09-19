#!/usr/bin/env bash
# Reference checkouts of separately maintained services, not ASS source code.
# The GPU circus lives elsewhere; we only keep its programme here.
set -uo pipefail

# Entries are "url" or "url|dir" to override the checkout directory.
PUBLIC_REPOS=(
  "git@github.com:stevelittlefish/ACE-Step-1.5-inference-server.git"
  "git@github.com:stevelittlefish/forced-aligner.git"
  "git@github.com:stevelittlefish/stable-audio-3-docker.git"
  # Preserve the existing lowercase checkout name.
  "git@github.com:stevelittlefish/YuE-inference-server.git|yue-inference-server"
  "git@github.com:stevelittlefish/stem-separator.git"
)

cd "$(dirname "$0")"

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

echo
total=$(( ${#PUBLIC_REPOS[@]} ))
if [ ${#failed[@]} -eq 0 ]; then
  echo "Child-service reference repositories are up to date (of $total known)."
else
  # One unreachable remote should not hide the fact that the others worked, so
  # problems are collected and reported here rather than aborting the loop.
  echo "Finished with ${#failed[@]} problem(s):"
  printf '  - %s\n' "${failed[@]}"
  exit 1
fi
