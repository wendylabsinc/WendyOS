#!/bin/sh
set -eu
: "${WENDY_VERSION:?Pass an exact Wendy release tag with --build-arg WENDY_VERSION=...}"
case "$WENDY_VERSION" in
    *[!a-zA-Z0-9._-]*|latest) echo 'WENDY_VERSION must be an exact release tag' >&2; exit 1 ;;
esac
case "${TARGETARCH:-}" in
    amd64|arm64) arch="$TARGETARCH" ;;
    *) echo 'Only linux/amd64 and linux/arm64 are supported' >&2; exit 1 ;;
esac
version="${WENDY_VERSION#v}"
archive="wendy-cli-linux-${arch}-${version}.tar.gz"
curl --fail --location --retry 3 --connect-timeout 30 --max-time 600 \
    "https://github.com/wendylabsinc/wendy-agent/releases/download/${WENDY_VERSION}/${archive}" \
    --output /tmp/wendy.tar.gz
if [ -n "${WENDY_SHA256:-}" ]; then
    printf '%s  /tmp/wendy.tar.gz\n' "$WENDY_SHA256" | sha256sum --check -
fi
tar -xzf /tmp/wendy.tar.gz -C /tmp "wendy-cli-linux-${arch}/wendy"
install -m 0755 "/tmp/wendy-cli-linux-${arch}/wendy" /usr/local/bin/wendy
installed="$(wendy --version)"
case "$installed" in
    "wendy version $version"|"wendy version $WENDY_VERSION") printf '%s\n' "$installed" ;;
    *) printf 'Unexpected CLI version: %s\n' "$installed" >&2; exit 1 ;;
esac
