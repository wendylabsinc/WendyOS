#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
APP_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$APP_ROOT"
xcodegen generate
if [[ -n "${WENDY_DEVELOPMENT_TEAM:-}" ]]; then
  xcodebuild -project WendyDesktop.xcodeproj -scheme WendyDesktop -configuration Debug -derivedDataPath Build/DerivedData DEVELOPMENT_TEAM="$WENDY_DEVELOPMENT_TEAM" "$@" build
else
  xcodebuild -project WendyDesktop.xcodeproj -scheme WendyDesktop -configuration Debug -derivedDataPath Build/DerivedData CODE_SIGNING_ALLOWED=NO "$@" build
fi
