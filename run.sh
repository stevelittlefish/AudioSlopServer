#!/bin/bash
# Run the Audio Slop Server outside a container, for development. In production
# we dockerise it (see the Deployment topology note in CLAUDE.md); this is the
# easier-to-test bare-metal path.
#
#   ./run.sh                       serve using ass.toml
#   ./run.sh -config other.toml    use a different config file
#
# The cd matters: ass.toml (and anything else relative it points at) is resolved
# from the repo root, so ASS must run from here regardless of where you invoke
# this script from.
cd "$(dirname "$0")" || exit 1

if [ ! -f ass.toml ] && [[ "$*" != *-config* ]]; then
  echo "No ass.toml here. Use the committed one, or point at another with:" >&2
  echo "  ./run.sh -config /path/to/ass.toml" >&2
  exit 1
fi

exec go run ./cmd/ass "$@"
