#!/bin/bash
# WendyOS User Setup - First Boot Service
# This script runs once on first boot to initialize user home directories
# on the persistent data partition.

set -e

SETUP_FLAG="/data/.wendyos-user-setup-done"
DATA_HOME="/data/home"
WENDY_HOME="/data/home/wendy"
WENDY_UID=1000
WENDY_GID=1000

# Check if already run
if [ -f "$SETUP_FLAG" ]; then
    echo "WendyOS user setup already completed. Skipping."
    exit 0
fi

echo "Running WendyOS user setup (first boot)..."

# Ensure /data is mounted
if ! mountpoint -q /data; then
    echo "ERROR: /data is not mounted. Cannot proceed."
    exit 1
fi

# Create /data/home if it doesn't exist
if [ ! -d "$DATA_HOME" ]; then
    echo "Creating $DATA_HOME..."
    mkdir -p "$DATA_HOME"
    chmod 0755 "$DATA_HOME"
fi

# Create wendy user home directory if it doesn't exist
if [ ! -d "$WENDY_HOME" ]; then
    echo "Creating home directory for wendy user at $WENDY_HOME..."
    mkdir -p "$WENDY_HOME"
    chmod 0755 "$WENDY_HOME"

    # Create .ssh directory
    mkdir -p "$WENDY_HOME/.ssh"
    chmod 0700 "$WENDY_HOME/.ssh"

    # Create minimal .bashrc that sources system profiles
    cat > "$WENDY_HOME/.bashrc" << 'EOF'
# WendyOS User Environment
# System defaults are in /etc/profile.d/ and update via OTA
# Add your personal customizations below
# (Login shells already source /etc/profile; no need to re-source here)

EOF
    chmod 0644 "$WENDY_HOME/.bashrc"

    # Create .profile for login shells
    cat > "$WENDY_HOME/.profile" << 'EOF'
# WendyOS User Profile
# This file is sourced by login shells

# Source .bashrc if it exists
if [ -f "$HOME/.bashrc" ]; then
    . "$HOME/.bashrc"
fi

EOF
    chmod 0644 "$WENDY_HOME/.profile"

    # Set ownership
    chown -R $WENDY_UID:$WENDY_GID "$WENDY_HOME"

    echo "wendy user home directory initialized at $WENDY_HOME"
else
    echo "wendy user home directory already exists at $WENDY_HOME"
fi

# Create flag file to indicate setup is complete
touch "$SETUP_FLAG"
echo "WendyOS user setup complete."

exit 0
