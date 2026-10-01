#!/usr/bin/env bash
# Cross-compile the inspection probe for the robot. No cgo, so this runs anywhere.
set -euo pipefail

arch="${1:-arm64}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

cd "$repo/go"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
  go build -ldflags="-s -w" -o "$here/robot-inspect" ./cmd/robot-inspect

printf 'built %s (%s)\n' "$here/robot-inspect" "$(du -h "$here/robot-inspect" | cut -f1)"
