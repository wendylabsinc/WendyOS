#!/bin/bash
set -euo pipefail
APP_ROOT="$1"
REPO_ROOT="$2"
cd "$REPO_ROOT"
# c-archive outputs are native to the build Mac. Clean Build/ before changing
# architecture. Dependencies and Go's cache are managed by the root go.mod.
export CGO_ENABLED=1
export MACOSX_DEPLOYMENT_TARGET=26.0
export CGO_CFLAGS="-O2 -g -mmacosx-version-min=26.0"
export CGO_CXXFLAGS="$CGO_CFLAGS"
export CGO_LDFLAGS="-mmacosx-version-min=26.0"
# Xcode's SDKROOT also reaches cgo; use the macOS SDK for all C sources.
export SDKROOT="$(xcrun --sdk macosx --show-sdk-path)"
# Rebuild only after inputs changed. A root go.mod/go.sum change invalidates all.
stamp="$APP_ROOT/Build/.go-inputs"
next="$APP_ROOT/Build/.go-inputs.next"
python3 "$APP_ROOT/Scripts/go-fingerprint.py" > "$next"
if cmp -s "$stamp" "$next" && test -s "$APP_ROOT/Build/libwendydesktop.a" && test -s "$APP_ROOT/Build/libwendyvpn.a" && test -s "$APP_ROOT/Build/libusb-1.0.dylib" && test -x "$APP_ROOT/Build/wendy"; then
  rm "$next"
  exit 0
fi
go build -buildmode=c-archive -o "$APP_ROOT/Build/libwendydesktop.a" ./go/macos/Bridge
go build -buildmode=c-archive -o "$APP_ROOT/Build/libwendyvpn.a" ./go/macos/VPNBridge
go build -o "$APP_ROOT/Build/wendy" ./go/cmd/wendy
USB_LIBRARY="$(pkg-config --variable=libdir libusb-1.0)/libusb-1.0.dylib"
install -m 755 "$USB_LIBRARY" "$APP_ROOT/Build/libusb-1.0.dylib"
install_name_tool -id '@rpath/libusb-1.0.dylib' "$APP_ROOT/Build/libusb-1.0.dylib"
mv "$next" "$stamp"
