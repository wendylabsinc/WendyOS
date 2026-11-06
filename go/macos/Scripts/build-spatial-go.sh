#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
APP_ROOT="${SRCROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$APP_ROOT/../.."
case "${PLATFORM_NAME:-xrsimulator}" in
  xros) sdk=xros; target=arm64-apple-xros26.0 ;;
  xrsimulator) sdk=xrsimulator; target=arm64-apple-xros26.0-simulator ;;
  *) echo "Unsupported spatial platform: $PLATFORM_NAME" >&2; exit 1 ;;
esac
output="$APP_ROOT/Build/$sdk"
mkdir -p "$output"
# Go's Apple mobile runtime provides the sandboxed Darwin implementation.
# Clang selects the actual visionOS SDK and Mach-O platform for all C objects.
export GOOS=ios GOARCH=arm64 CGO_ENABLED=1
export SDKROOT="$(xcrun --sdk "$sdk" --show-sdk-path)"
export CC="$(xcrun --sdk "$sdk" --find clang) -target $target"
export CGO_CFLAGS="-isysroot $SDKROOT -O2"
export CGO_LDFLAGS="-isysroot $SDKROOT -lresolv"
export CGO_CXXFLAGS="$CGO_CFLAGS"
go build -buildmode=c-archive -o "$output/libwendydesktop.a" ./go/macos/Bridge
