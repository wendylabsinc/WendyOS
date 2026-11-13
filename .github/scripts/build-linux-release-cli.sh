#!/usr/bin/env bash
# Exact Linux release CLI recipe, shared by build.yml and the native PR package gate.
set -euo pipefail
[[ $# == 3 ]] || { echo 'usage: build-linux-release-cli.sh VERSION ARCH NEW_BINARY_PATH' >&2; exit 2; }
release_version=$1
release_arch=$2
[[ "$release_version" =~ ^[0-9][A-Za-z0-9.+_~-]*$ ]] || { echo 'invalid version' >&2; exit 2; }
[[ "$release_arch" == amd64 || "$release_arch" == arm64 ]]
[[ $(go env GOHOSTOS) == linux && $(go env GOHOSTARCH) == "$release_arch" ]] || {
  echo 'release CLI requires native Linux, without emulation' >&2; exit 1;
}
[[ ! -e "$3" && ! -L "$3" ]] || { echo 'output must be a new binary path' >&2; exit 2; }
release_output=$(realpath -m "$3")
release_repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
release_work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-release-cli.XXXXXXXX")
trap 'rm -rf -- "$release_work"' EXIT
cd "$release_work"
# The release workflow already pins this upstream libusb archive.
release_libusb=1.0.29
release_libusb_sha256=5977fc950f8d1395ccea9bd48c06b3f808fd3c2c961b44b0c2e6e29fc3a70a85
curl --fail --silent --show-error --location --max-time 120 \
  --output libusb.tar.bz2 "https://github.com/libusb/libusb/releases/download/v${release_libusb}/libusb-${release_libusb}.tar.bz2"
printf '%s  libusb.tar.bz2\n' "$release_libusb_sha256" | sha256sum -c -
tar -xjf libusb.tar.bz2
cd "libusb-$release_libusb"
mkdir -p "$release_work/kernel-headers"
cp -r /usr/include/linux "$release_work/kernel-headers/linux"
cp -r "/usr/include/$(gcc -print-multiarch)/asm" "$release_work/kernel-headers/asm"
cp -r /usr/include/asm-generic "$release_work/kernel-headers/asm-generic"
CC=musl-gcc CFLAGS="-I$release_work/kernel-headers" ./configure --disable-udev --disable-shared \
  --enable-static --prefix="$release_work/libusb-prefix"
make -j"$(nproc)"
make install
cd "$release_repo/go"
release_ldflags="-s -w -X github.com/wendylabsinc/wendy/go/internal/shared/version.Version=$release_version"
# Include the owned prefix in Go's cgo cache key. Changing PKG_CONFIG_PATH
# alone can reuse linker directives pointing into a deleted previous build.
CGO_ENABLED=1 CC=musl-gcc GOOS=linux GOARCH="$release_arch" \
  PKG_CONFIG_PATH="$release_work/libusb-prefix/lib/pkgconfig" \
  CGO_LDFLAGS="-O2 -g -L$release_work/libusb-prefix/lib" \
  go build -tags netgo,osusergo \
  -ldflags "$release_ldflags -linkmode external -extldflags -static" \
  -o "$release_work/wendy" ./cmd/wendy
if ldd "$release_work/wendy" >/dev/null 2>&1; then
  echo 'release CLI unexpectedly links dynamic libraries' >&2
  exit 1
fi
[[ $("$release_work/wendy" --version) == "wendy version $release_version" ]]
mkdir -p "$(dirname -- "$release_output")"
install -m755 "$release_work/wendy" "$release_output"
