#!/usr/bin/env bash
#
# Pull every backend image referenced in ass.toml into the local Docker store.
#
# ASS never auto-pulls — it just consumes backend images by name and refuses if
# one is missing (a surprise multi-GB download mid-request is rude). So run this
# on the GPU host to fetch/update the backend images before starting ASS, and
# again whenever you want the latest ones.
#
#   ./pull-services.sh                 # read ass.toml (the default)
#   ./pull-services.sh ass.dev.toml    # or a different config
#
# The image list is read straight from the config, so adding a service never
# means editing this script. Images with no registry path (a local build tag like
# "foo:local") are skipped — there's nothing to pull; build those yourself.

set -uo pipefail
cd "$(dirname "$0")" || exit 1

CONFIG="${1:-ass.toml}"
if [ ! -f "$CONFIG" ]; then
  echo "No such config: $CONFIG" >&2
  exit 1
fi

# Pull the quoted value out of every `image = "..."` line.
mapfile -t IMAGES < <(grep -E '^[[:space:]]*image[[:space:]]*=' "$CONFIG" \
  | sed -E 's/.*=[[:space:]]*"([^"]+)".*/\1/')

if [ ${#IMAGES[@]} -eq 0 ]; then
  echo "No images found in $CONFIG" >&2
  exit 1
fi

failed=()
for img in "${IMAGES[@]}"; do
  # A registry ref has a path (a slash); a bare local tag doesn't.
  if [[ "$img" != */* ]]; then
    echo "== skip  $img  (local build tag — nothing to pull)"
    continue
  fi
  echo "== pull  $img"
  docker pull "$img" || failed+=("$img")
done

echo
if [ ${#failed[@]} -eq 0 ]; then
  echo "Done — ${#IMAGES[@]} image(s) referenced in $CONFIG are present locally."
else
  echo "Failed to pull ${#failed[@]}:"
  printf '  - %s\n' "${failed[@]}"
  echo "(Private GHCR package? 'docker login ghcr.io' first. Not published yet?"
  echo " cut a release in that backend's repo with ./make_release.sh)"
  exit 1
fi
