#!/bin/bash

## There are times that the `./dev-update-agent.sh` doesn't work, so we need to use this script to update the agent manually.
read -p "Enter hostname [wendyos-humble-pepper.local]: " HOSTNAME
HOSTNAME=${HOSTNAME:-wendyos-humble-pepper.local}
USER=edge
PASSWORD=edge


# Locally build the WendyCLI binary for the device's architecture (aarch64-swift-linux-musl) with debug not release mode.
swiftly run swift build --product wendy-agent --swift-sdk aarch64-swift-linux-musl 

# SCP the binary to the device (use -v for verbose progress)
echo "📦 Copying wendy-agent to ${USER}@${HOSTNAME}..."
sshpass -p "${PASSWORD}" scp -v -o StrictHostKeyChecking=no .build/aarch64-swift-linux-musl/debug/wendy-agent "${USER}@${HOSTNAME}:~/"

# SSH into the device and move/restart
# Right now it still has to be called `edge-agent` because the service is called `edge-agent` in the Yocto image, we haven't renamed it yet.
echo "🚀 Installing and restarting edge-agent service on ${HOSTNAME}..."
sshpass -p "${PASSWORD}" ssh -o StrictHostKeyChecking=no "${USER}@${HOSTNAME}" "echo ${PASSWORD} | sudo -S mv ./wendy-agent /usr/local/bin/wendy-agent && echo ${PASSWORD} | sudo -S systemctl restart edge-agent && echo '✅ Agent updated and restarted successfully!'"