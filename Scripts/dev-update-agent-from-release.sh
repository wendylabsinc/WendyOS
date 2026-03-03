#!/bin/bash
set -e

## Updates the wendy-agent on a device using the latest GitHub release instead of building locally.

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

SSH_USER=${SSH_USER:-root}
INCLUDE_PRERELEASE=${INCLUDE_PRERELEASE:-false}

# Create temp directory for download
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

echo "Fetching latest release from GitHub..."

# Get the latest release info from GitHub API
if [ "$INCLUDE_PRERELEASE" = "true" ]; then
    # Get all releases and pick the first one (includes prereleases)
    RELEASE_INFO=$(curl -s "https://api.github.com/repos/wendylabsinc/wendy-agent/releases" | head -c 100000)
    RELEASE_TAG=$(echo "$RELEASE_INFO" | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
else
    # Get only the latest stable release
    RELEASE_INFO=$(curl -s "https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest")
    RELEASE_TAG=$(echo "$RELEASE_INFO" | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
fi

if [ -z "$RELEASE_TAG" ]; then
    echo "Error: Could not fetch release information from GitHub"
    exit 1
fi

echo "Latest release: $RELEASE_TAG"

# Construct download URL for aarch64 (WendyOS devices are ARM)
ASSET_NAME="wendy-agent-linux-arm64-${RELEASE_TAG}.tar.gz"
DOWNLOAD_URL="https://github.com/wendylabsinc/wendy-agent/releases/download/${RELEASE_TAG}/${ASSET_NAME}"

echo "Downloading $ASSET_NAME..."
curl -L -o "$TEMP_DIR/$ASSET_NAME" "$DOWNLOAD_URL"

echo "Extracting binary..."
tar -xzf "$TEMP_DIR/$ASSET_NAME" -C "$TEMP_DIR"

# Find the binary in the extracted directory
BINARY_PATH="$TEMP_DIR/wendy-agent-linux-arm64/wendy-agent"
if [ ! -f "$BINARY_PATH" ]; then
    echo "Error: Could not find wendy-agent binary in extracted archive"
    exit 1
fi

echo "Uploading to $HOSTNAME..."
scp "$BINARY_PATH" "${SSH_USER}@${HOSTNAME}:~/"

echo "Installing and restarting service..."
# Right now it still has to be called `edge-agent` because the service is called `edge-agent` in the Yocto image
ssh "${SSH_USER}@${HOSTNAME}" "sudo mv ./wendy-agent /usr/local/bin/wendy-agent; sudo systemctl restart edge-agent"

echo "Done! Updated wendy-agent to $RELEASE_TAG on $HOSTNAME"
