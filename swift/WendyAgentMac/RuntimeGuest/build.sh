#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$ROOT/../../.." && pwd)"
OUT="${WENDY_RUNTIME_GUEST_OUT:-$ROOT/../Resources/runtime}"
PLATFORM="${WENDY_RUNTIME_PLATFORM:-linux/arm64}"
ARCH="${PLATFORM##*/}"
ROOTFS="$(mktemp -d "${TMPDIR:-/tmp}/wendy-runtime-rootfs.XXXXXX")"

case "$ARCH" in
  arm64) GOARCH=arm64 ;;
  amd64) GOARCH=amd64 ;;
  *) echo "unsupported runtime architecture: $ARCH" >&2; exit 64 ;;
esac

command -v cpio >/dev/null || { echo "cpio is required" >&2; exit 69; }

mkdir -p "$OUT"
GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o "$ROOT/wendy-runtime-guest-proxy" \
  "$REPO_ROOT/go/cmd/wendy-runtime-guest-proxy"
cleanup() {
  rm -f "$ROOT/wendy-runtime-guest-proxy"
  rm -f "$OUT/.vmlinuz-$ARCH.tmp"
  case "$ROOTFS" in
    "${TMPDIR:-/tmp}"/wendy-runtime-rootfs.*) rm -rf "$ROOTFS" ;;
  esac
}
trap cleanup EXIT

if command -v buildctl >/dev/null && buildctl debug workers >/dev/null 2>&1; then
  buildctl build \
    --frontend dockerfile.v0 \
    --local context="$ROOT" \
    --local dockerfile="$ROOT" \
    --opt platform="$PLATFORM" \
    --output "type=local,dest=$ROOTFS"
elif command -v docker >/dev/null && docker buildx version >/dev/null 2>&1; then
  # Bootstrap path for contributors who have not started Wendy's own daemon
  # yet. buildx is still BuildKit; release jobs use buildctl directly.
  docker buildx build \
    --platform "$PLATFORM" \
    --output "type=local,dest=$ROOTFS" \
    "$ROOT"
else
  echo "a reachable BuildKit daemon (buildctl or docker buildx) is required" >&2
  exit 69
fi

# Keep the kernel and its modules from the same signed Alpine package set. The
# modules stay in the initramfs; the kernel is booted directly by VZ and is not
# duplicated inside the archive. Alpine arm64 packages vmlinuz as a compressed
# EFI/zboot wrapper. VZLinuxBootLoader needs the raw arm64 Image embedded in
# that wrapper, identified by its gzip member and validated by the ARMd header.
VMLINUX="$ROOTFS/boot/vmlinuz-virt"
if [[ "$ARCH" == "arm64" ]]; then
  GZIP_MATCH="$(LC_ALL=C grep -aobm1 $'\x1f\x8b\x08' "$VMLINUX")"
  GZIP_OFFSET="${GZIP_MATCH%%:*}"
  if [[ -z "$GZIP_OFFSET" ]]; then
    echo "could not locate the compressed arm64 Image in $VMLINUX" >&2
    exit 65
  fi

  set +e
  tail -c "+$((GZIP_OFFSET + 1))" "$VMLINUX" \
    | gzip -dc >"$OUT/.vmlinuz-$ARCH.tmp" 2>/dev/null
  GZIP_STATUS=${PIPESTATUS[1]}
  set -e
  # gzip returns 2 for the expected PE data following its member.
  if [[ "$GZIP_STATUS" -ne 0 && "$GZIP_STATUS" -ne 2 ]]; then
    echo "could not extract the raw arm64 Image from $VMLINUX" >&2
    exit 65
  fi
  KERNEL_MAGIC="$(dd if="$OUT/.vmlinuz-$ARCH.tmp" bs=1 skip=56 count=4 2>/dev/null)"
  if [[ "$KERNEL_MAGIC" != "ARMd" ]]; then
    echo "extracted kernel does not contain the arm64 Image header" >&2
    exit 65
  fi
  mv "$OUT/.vmlinuz-$ARCH.tmp" "$OUT/vmlinuz-$ARCH"
else
  cp "$VMLINUX" "$OUT/vmlinuz-$ARCH"
fi

(
  cd "$ROOTFS"
  find . -path ./boot -prune -o -print0 | cpio --null -o --format=newc
) | gzip -9 >"$OUT/initramfs-$ARCH.img"

echo "Built $OUT/initramfs-$ARCH.img"
echo "Built $OUT/vmlinuz-$ARCH"
