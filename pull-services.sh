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
# The image list comes from ASS itself (`ass -print-images`), which parses the
# config with the same code the server uses — so adding a service never means
# editing this script, and `disabled = true` services drop out for free (no
# second, hand-rolled TOML parser to keep in sync). Images with no registry path
# (a local build tag like "foo:local") are skipped — nothing to pull; build those
# yourself.

set -uo pipefail
cd "$(dirname "$0")" || exit 1

CONFIG="${1:-ass.toml}"
if [ ! -f "$CONFIG" ]; then
  echo "No such config: $CONFIG" >&2
  exit 1
fi

# Ask ASS which images this config needs. `go run` builds first, so this also
# fails loudly on a config error (same validation the server runs) before we pull
# anything. Capture into a variable so we see go run's real exit status — a
# process substitution would hide it and hand mapfile an empty list instead.
# Logs go to stderr; only the image list lands on stdout.
if ! image_list="$(go run ./cmd/ass -config "$CONFIG" -print-images)"; then
  echo "Could not read images from $CONFIG (config error, or the build failed)." >&2
  exit 1
fi
mapfile -t IMAGES < <(printf '%s\n' "$image_list" | grep -v '^[[:space:]]*$')

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
