#!/bin/bash

## There are times that the `./dev-update-agent.sh` doesn't work, so we need to use this script to update the agent manually.
read -p "Enter hostname [wendyos-merry-aurora.local]: " HOSTNAME
HOSTNAME=${HOSTNAME:-wendyos-merry-aurora.local}
USER=edgeos


# Locally build the WendyCLI binary for the device's architecture (aarch64-swift-linux-musl) with debug not release mode.
swiftly run swift build --product wendy-agent --swift-sdk aarch64-swift-linux-musl && .build/arm64-apple-macosx/debug/wendy

# SCP the binary to the device
scp .build/aarch64-swift-linux-musl/debug/wendy-agent "${USER}@${HOSTNAME}:~/"

# SSH into the device and move/restart
# Right now it still has to be called `edge-agent` because the service is called `edge-agent` in the Yocto image, we haven't renamed it yet.
ssh "${USER}@${HOSTNAME}" "sudo mv ./wendy-agent /usr/local/bin/wendy-agent; sudo systemctl restart edge-agent"