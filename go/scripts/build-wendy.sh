#!/usr/bin/env bash
# Build (or install) the wendy CLI. Thor USB recovery flashing (gousb) needs
# libusb on macOS and Linux; on macOS it is linked statically so the shipped
# binary carries no /opt/homebrew (or other) libusb dylib dependency. Linux dev
# builds link the system libusb dynamically (release builds are fully static —
# see .github/workflows/build.yml). Windows needs no libusb (unsupported stub).
#
# Usage (run from the repo root):
#   go/scripts/build-wendy.sh [output-path]  # build to output-path (default ./bin/wendy)
#   go/scripts/build-wendy.sh --install      # go install into GOBIN
set -euo pipefail
cd "$(dirname "$0")/.."   # the go module root

LDFLAGS="-s -w -X github.com/wendylabsinc/wendy/go/internal/shared/version.Version=${VERSION:-dev}"

if [[ "$(uname -s)" != "Darwin" ]]; then
    # Linux links the system libusb dynamically (dev builds; releases are static
    # via CI); other platforms need no libusb (Thor path compiles to a stub).
    if [[ "$(uname -s)" == "Linux" ]] && ! pkg-config --exists libusb-1.0 2>/dev/null; then
        echo "error: libusb-1.0 dev headers not found (apt install libusb-1.0-0-dev / dnf install libusb1-devel)"
        exit 1
    fi
    if [[ "${1:-}" == "--install" ]]; then exec go install -ldflags "$LDFLAGS" ./cmd/wendy; fi
    exec go build -ldflags "$LDFLAGS" -o "${1:-bin/wendy}" ./cmd/wendy
fi

# --- macOS: static libusb -------------------------------------------------------
LIBUSB_PREFIX="$(brew --prefix libusb 2>/dev/null || echo /opt/homebrew/opt/libusb)"
ARCHIVE="$LIBUSB_PREFIX/lib/libusb-1.0.a"
HEADERS="$LIBUSB_PREFIX/include/libusb-1.0"
[[ -f "$ARCHIVE" ]] || { echo "error: $ARCHIVE not found (brew install libusb)"; exit 1; }

# gousb links libusb via `#cgo pkg-config: libusb-1.0`. To force the STATIC archive
# we (a) neutralize pkg-config so it can't re-introduce the .dylib search path, and
# (b) point the linker at a dir containing ONLY the .a, so the macOS linker has no
# .dylib to prefer.
STATICDIR="$(mktemp -d)"
trap 'rm -rf "$STATICDIR"' EXIT
ln -s "$ARCHIVE" "$STATICDIR/libusb-1.0.a"

export CGO_ENABLED=1
export PKG_CONFIG=/usr/bin/true
export CGO_CFLAGS="-I$HEADERS"
export CGO_LDFLAGS="-L$STATICDIR -lusb-1.0 -lobjc -Wl,-framework,IOKit -Wl,-framework,CoreFoundation -Wl,-framework,Security"

if [[ "${1:-}" == "--install" ]]; then
    go install -ldflags "$LDFLAGS" ./cmd/wendy
    echo "installed wendy (static libusb) into ${GOBIN:-$(go env GOPATH)/bin}"
    exit 0
fi
OUT="${1:-bin/wendy}"
mkdir -p "$(dirname "$OUT")"
go build -ldflags "$LDFLAGS" -o "$OUT" ./cmd/wendy
echo "built $OUT (static libusb)"
# Fail loudly if a libusb dylib snuck back in.
if otool -L "$OUT" | grep -qi 'libusb'; then
    echo "ERROR: $OUT still has a dynamic libusb dependency:"; otool -L "$OUT" | grep -i usb
    exit 1
fi
echo "verified: no dynamic libusb dependency"
