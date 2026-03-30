#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  echo "Usage: $0 [--host <hostname/IP>] [arm64|aarch64|amd64|x86_64]"
  echo "  --host   Target device hostname or IP address"
  echo "  Default arch: arm64"
  exit 1
}

HOST=""
ARCH=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      HOST="${2:?--host requires a value}"
      shift 2
      ;;
    arm64|aarch64|amd64|x86_64)
      ARCH="$1"
      shift
      ;;
    *)
      usage
      ;;
  esac
done

ARCH="${ARCH:-arm64}"
VERSION="${VERSION:-dev}"

case "$ARCH" in
  arm64|aarch64)
    GOARCH=arm64
    MAKE_TARGET=build-agent-linux-arm64
    BINARY="$GO_DIR/bin/wendy-agent-linux-arm64"
    ;;
  amd64|x86_64)
    GOARCH=amd64
    MAKE_TARGET=build-agent-linux-amd64
    BINARY="$GO_DIR/bin/wendy-agent-linux-amd64"
    ;;
esac

echo "Cross-compiling wendy-agent for linux/$GOARCH..."
make -C "$GO_DIR" VERSION="$VERSION" "$MAKE_TARGET"

DEVICE_FLAG=()
if [[ -n "$HOST" ]]; then
  DEVICE_FLAG=(--device "$HOST")
fi

echo "Deploying to device via 'wendy device update'..."
go -C "$GO_DIR" run ./cmd/wendy device update --binary "$BINARY" "${DEVICE_FLAG[@]}"
