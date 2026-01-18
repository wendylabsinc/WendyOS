#!/bin/bash
set -e

# Get hostname from first argument or use default
if [ -n "$1" ]; then
    HOSTNAME="$1"
else
    HOSTNAME="ubuntu.orb.local"
fi

# Add .local suffix if missing
if [[ ! "$HOSTNAME" == *.local ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

swiftly run swift build --product wendy-agent --swift-sdk aarch64-swift-linux-musl && .build/arm64-apple-macosx/debug/wendy agent update --binary .build/aarch64-swift-linux-musl/debug/wendy-agent --device "${HOSTNAME}"
