#!/usr/bin/env bash
# Build the release CLI on newer Macs without importing their libusb ABI.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: build-macos-release-cli.sh NEW_BINARY_PATH' >&2; exit 2; }
[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || {
  echo 'release CLI requires native macOS ARM64' >&2; exit 1;
}
export MACOSX_DEPLOYMENT_TARGET=26.0
[[ ! -e "$1" && ! -L "$1" ]] || { echo 'output must be a new binary path' >&2; exit 2; }
mkdir -p "$(dirname -- "$1")"
release_output="$(cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")"
release_repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
release_work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-macos-release.XXXXXX")
trap 'rm -rf -- "$release_work"' EXIT
cd "$release_work"

# Match the pinned libusb source used by the Linux release build.
release_libusb=1.0.29
release_libusb_sha256=5977fc950f8d1395ccea9bd48c06b3f808fd3c2c961b44b0c2e6e29fc3a70a85
curl --fail --silent --show-error --location --max-time 120 \
  --output libusb.tar.bz2 "https://github.com/libusb/libusb/releases/download/v${release_libusb}/libusb-${release_libusb}.tar.bz2"
printf '%s  libusb.tar.bz2\n' "$release_libusb_sha256" | shasum -a 256 -c -
tar -xjf libusb.tar.bz2
cd "libusb-$release_libusb"
# Autoconf's link check can detect pipe2 in the newer SDK even though the
# deployment OS lacks it. Select libusb's pipe/fcntl fallback explicitly.
ac_cv_func_pipe2=no \
  CFLAGS="-O2 -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET -Werror=unguarded-availability-new" \
  LDFLAGS="-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
  ./configure --disable-shared --enable-static --prefix="$release_work/libusb-prefix"
make -j"$(sysctl -n hw.ncpu)"
make install

cd "$release_repo"
LIBUSB_PREFIX="$release_work/libusb-prefix" bash go/scripts/build-wendy.sh "$release_output"

# Check the actual executable, including symbols contributed by static libraries.
minimum=$(xcrun vtool -show-build "$release_output" | awk '$1 == "minos" { print $2 }')
[[ "$minimum" == "$MACOSX_DEPLOYMENT_TARGET" ]] || {
  echo "unexpected macOS minimum: $minimum (expected $MACOSX_DEPLOYMENT_TARGET)" >&2; exit 1;
}
imports=$(nm -u "$release_output")
if grep -Eq '(^|[[:space:]])_pipe2$' <<< "$imports"; then
  echo 'release CLI still imports pipe2, unavailable on macOS 26' >&2
  exit 1
fi
"$release_output" --version
for shell in bash zsh fish; do
  "$release_output" completion "$shell" > /dev/null
done
