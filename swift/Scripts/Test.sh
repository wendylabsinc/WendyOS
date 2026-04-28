#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
SCRATCH_PATH="${SCRATCH_PATH:-$SWIFT_DIR/Build/SwiftPM}"

xcbeautify_or_cat() {
  if command -v xcbeautify >/dev/null 2>&1; then
    if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
      xcbeautify --renderer github-actions
    else
      xcbeautify
    fi
  else
    cat
  fi
}

mkdir -p "$SCRATCH_PATH"
cd "$SWIFT_DIR/WendyAgentCore"
swift test --scratch-path "$SCRATCH_PATH" | xcbeautify_or_cat
