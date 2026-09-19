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
#
# We get that list one of two ways, auto-detected (override with ASS_IMAGES_VIA=go
# or =docker):
#   - docker run ass:local ...    the default when the image is built — the deploy
#                                 host needs only Docker, not Go (the binary was
#                                 built inside the image). The config is bind-mounted
#                                 in by absolute path, so it can live anywhere.
#   - go run ./cmd/ass ...        fallback when there's no ass:local image but a Go
#                                 toolchain is present (dev boxes mid-iteration).
# Build the image first with `docker compose build` (or `docker build`).

set -uo pipefail
cd "$(dirname "$0")" || exit 1

CONFIG="${1:-ass.toml}"
if [ ! -f "$CONFIG" ]; then
  echo "No such config: $CONFIG" >&2
  exit 1
fi

ASS_IMAGE="${ASS_IMAGE:-ass:local}"

# print_images_via_go / _docker each emit the enabled services' images on stdout,
# or fail non-zero on a config/build error. Either way it's the SAME config.Load
# the server runs, so validation and the disabled filter are identical.
print_images_via_go() { go run ./cmd/ass -config "$CONFIG" -print-images; }
print_images_via_docker() {
  # Mount the config read-only and let the containerised binary read it. Absolute
  # path required for the bind mount; the entrypoint is `ass`, so the args follow.
  docker run --rm -v "$(realpath "$CONFIG")":/cfg/config.toml:ro \
    "$ASS_IMAGE" -config /cfg/config.toml -print-images
}

# Pick a method. Honour an explicit ASS_IMAGES_VIA; otherwise default to the
# container image (what the deploy host actually runs, and it needs only Docker),
# falling back to `go run` only when the image isn't built but Go is around.
method="${ASS_IMAGES_VIA:-}"
if [ -z "$method" ]; then
  if docker image inspect "$ASS_IMAGE" >/dev/null 2>&1; then
    method=docker
  elif command -v go >/dev/null 2>&1; then
    method=go
  else
    echo "Need a way to read the config: build the $ASS_IMAGE image first" >&2
    echo "(docker compose build), or install Go. Then re-run." >&2
    exit 1
  fi
fi

# Capture into a variable so the real exit status survives — a process
# substitution would hide it and hand mapfile an empty list instead.
case "$method" in
  go)     image_list="$(print_images_via_go)"     || method_err=1 ;;
  docker) image_list="$(print_images_via_docker)" || method_err=1 ;;
  *) echo "ASS_IMAGES_VIA must be 'go' or 'docker', not '$method'" >&2; exit 1 ;;
esac
if [ -n "${method_err:-}" ]; then
  echo "Could not read images from $CONFIG via $method (config error, or the" >&2
  echo "build/image is missing). For the container path, build $ASS_IMAGE first." >&2
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
