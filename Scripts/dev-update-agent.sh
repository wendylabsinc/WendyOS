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

# Get architecture from second argument or prompt
if [ -n "$2" ]; then
    ARCH="$2"
else
    read -p "Enter architecture [aarch64/x86_64] (default: aarch64): " ARCH
    ARCH=${ARCH:-aarch64}
fi

# Validate architecture
case "$ARCH" in
    aarch64|x86_64)
        ;;
    *)
        echo "Error: Unsupported architecture '$ARCH'. Use 'aarch64' or 'x86_64'."
        exit 1
        ;;
esac

SWIFT_SDK="${ARCH}-swift-linux-musl"

swiftly run swift build --scratch-path .agent-build --product wendy-agent --swift-sdk "$SWIFT_SDK" && wendy device update --binary ".agent-build/${SWIFT_SDK}/debug/wendy-agent" --device "${HOSTNAME}"
