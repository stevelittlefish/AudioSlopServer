#!/bin/bash
# Build the mock-backend image (ass-mockbackend:local) that ASS starts during
# development in place of a real, GPU-hungry audio service.
#
# We compile a static binary on the host (CGO off) and copy it onto a slim base,
# so there's no Go toolchain or module download inside the image build.
set -euo pipefail
cd "$(dirname "$0")/.."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> Building static mockbackend binary"
CGO_ENABLED=0 go build -o "$tmp/mockbackend" ./cmd/mockbackend
cp cmd/mockbackend/Dockerfile "$tmp/"

echo "==> docker build -t ass-mockbackend:local"
docker build -t ass-mockbackend:local "$tmp"

echo "==> Done. Image: ass-mockbackend:local"
