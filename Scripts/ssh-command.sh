#!/bin/bash
set -e

## Run a command on a WendyOS device via SSH
## Usage: ./ssh-command.sh <hostname> <command>
## Example: ./ssh-command.sh wendyos-diligent-vessel "journalctl -u edge-agent -n 50"

if [ -z "$1" ] || [ -z "$2" ]; then
    echo "Usage: $0 <hostname> <command>"
    echo "Example: $0 wendyos-diligent-vessel \"journalctl -u edge-agent -n 50\""
    exit 1
fi

HOSTNAME="$1"
COMMAND="$2"
SSH_USER=${SSH_USER:-root}

# Ensure hostname ends with .local for mDNS resolution
if [[ ! "$HOSTNAME" =~ \.local$ ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

ssh "${SSH_USER}@${HOSTNAME}" "$COMMAND"
