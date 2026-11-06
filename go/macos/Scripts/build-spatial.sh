#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
APP_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
xcodegen generate --spec "$APP_ROOT/spatial.yml"
xcodebuild -project "$APP_ROOT/WendySpatial.xcodeproj" -scheme WendySpatial \
  -configuration Debug -destination 'generic/platform=visionOS Simulator' \
  -derivedDataPath "$APP_ROOT/Build/SpatialDerivedData" CODE_SIGNING_ALLOWED=NO build
