#!/usr/bin/env bash
# Integration test: requires real syft + go on PATH. Skips if syft is absent.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
SCRIPT="$HERE/../generate-sbom.sh"
command -v syft >/dev/null 2>&1 || { echo "SKIP: syft not installed"; exit 0; }
# syft is the optional real tool (skip above); go + jq are hard prerequisites
# for this test — fail with a clear message rather than a cryptic
# "command not found" mid-run under set -e.
for tool in go jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: required tool '$tool' not installed"; exit 1; }
done
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# Build a real wendy binary to catalog. The CLI links libusb via cgo on both
# macOS and Linux (Thor USB recovery flashing uses gousb, guarded by
# `//go:build darwin || linux`), so CGO must be enabled — mirroring CI's
# build-go-cli-linux / build-go-macos jobs, both of which build with
# CGO_ENABLED=1 against libusb. libusb dev headers must be on PATH (the
# sbom.yml workflow installs libusb-1.0-0-dev + pkg-config on Linux; macOS
# runners have libusb via Homebrew).
( cd "$ROOT/go" && CGO_ENABLED=1 go build -o "$TMP/wendy" ./cmd/wendy )

bash "$SCRIPT" binary "$TMP/wendy" "$TMP/wendy.spdx.json"
jq -e '.spdxVersion and (.packages | length > 0)' "$TMP/wendy.spdx.json" >/dev/null \
  || { echo "FAIL: binary SBOM missing packages"; exit 1; }

bash "$SCRIPT" source "$TMP/src.spdx.json" --repo-root "$ROOT"
jq -e '.spdxVersion' "$TMP/src.spdx.json" >/dev/null || { echo "FAIL: source SBOM invalid"; exit 1; }
# Excludes honored: no Examples/ file paths leak in.
if jq -e '[.. | .fileName? // empty] | map(select(startswith("Examples/"))) | length > 0' \
     "$TMP/src.spdx.json" >/dev/null 2>&1; then
  echo "FAIL: Examples/ not excluded"; exit 1
fi
echo "ok - integration"
