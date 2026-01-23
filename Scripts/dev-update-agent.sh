#!/bin/bash
set -e

# Get hostname from first argument or prompt
if [ -n "$1" ]; then
    HOSTNAME="$1"
else
    read -p "Enter hostname [wendyos-merry-aurora.local]: " HOSTNAME
    HOSTNAME=${HOSTNAME:-wendyos-merry-aurora.local}
fi

# Add .local suffix if missing
if [[ ! "$HOSTNAME" == *.local ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

swiftly run swift build --scratch-path .agent-build --product wendy-agent --swift-sdk aarch64-swift-linux-musl && wendy device update --binary .agent-build/aarch64-swift-linux-musl/debug/wendy-agent --device "${HOSTNAME}"
