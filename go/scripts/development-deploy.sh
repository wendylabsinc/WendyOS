#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Defaults
DEFAULT_HOSTNAME="wendyos-jolly-cedar.local"
DEFAULT_USER="edge"
DEFAULT_PASSWORD="edge"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Build and deploy the Go wendy-agent to a WendyOS device.

Options:
  -h, --hostname HOST    Device hostname (default: $DEFAULT_HOSTNAME)
  -u, --user USER        SSH username (default: $DEFAULT_USER)
  -p, --password PASS    SSH password (default: $DEFAULT_PASSWORD)
  --help                 Show this help message

Examples:
  $(basename "$0")
  $(basename "$0") -h wendyos-merry-aurora.local
  $(basename "$0") -h wendyos-merry-aurora.local -u root -p wendy
EOF
    exit 0
}

HOSTNAME="$DEFAULT_HOSTNAME"
SSH_USER="$DEFAULT_USER"
SSH_PASS="$DEFAULT_PASSWORD"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname) HOSTNAME="$2"; shift 2 ;;
        -u|--user)     SSH_USER="$2"; shift 2 ;;
        -p|--password) SSH_PASS="$2"; shift 2 ;;
        --help)        usage ;;
        *)             echo "Unknown option: $1"; usage ;;
    esac
done

# Add .local suffix if missing.
if [[ "$HOSTNAME" != *.local ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

SSH_CMD="sshpass -p ${SSH_PASS} ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ${SSH_USER}@${HOSTNAME}"
SCP_CMD="sshpass -p ${SSH_PASS} scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

echo "==> Building wendy-agent for linux/arm64..."
cd "$PROJECT_DIR"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o wendy-agent-linux-arm64 ./cmd/wendy-agent
echo "    Built: wendy-agent-linux-arm64 ($(du -h wendy-agent-linux-arm64 | cut -f1))"

echo "==> Stopping agent services on ${HOSTNAME}..."
$SSH_CMD "sudo systemctl stop edge-agent 2>/dev/null || true; sudo systemctl stop wendy-agent 2>/dev/null || true"

echo "==> Removing old agent binaries..."
$SSH_CMD "sudo rm -f /usr/local/bin/wendy-agent /usr/local/bin/edge-agent /usr/bin/wendy-agent /usr/bin/edge-agent 2>/dev/null || true"

echo "==> Copying new binary to device..."
$SCP_CMD wendy-agent-linux-arm64 "${SSH_USER}@${HOSTNAME}:~/wendy-agent"

echo "==> Installing and starting wendy-agent..."
$SSH_CMD "sudo mv ~/wendy-agent /usr/local/bin/wendy-agent && sudo chmod +x /usr/local/bin/wendy-agent"

# Determine which systemd service exists on this device.
# Newer images use wendy-agent.service, older ones use edge-agent.service.
ACTIVE_SERVICE=$($SSH_CMD "if systemctl cat wendy-agent.service >/dev/null 2>&1; then echo wendy-agent; else echo edge-agent; fi")
echo "    Using systemd service: ${ACTIVE_SERVICE}"

# Create a systemd override so the service runs our new binary from /usr/local/bin.
$SSH_CMD "sudo mkdir -p /etc/systemd/system/${ACTIVE_SERVICE}.service.d && \
sudo tee /etc/systemd/system/${ACTIVE_SERVICE}.service.d/override.conf > /dev/null <<'UNIT'
[Service]
ExecStart=
ExecStart=/usr/local/bin/wendy-agent
UNIT
sudo systemctl daemon-reload && sudo systemctl restart ${ACTIVE_SERVICE}"

echo "==> Verifying agent is running..."
sleep 2
$SSH_CMD "systemctl is-active ${ACTIVE_SERVICE}"

# Clean up local build artifact.
rm -f "$PROJECT_DIR/wendy-agent-linux-arm64"

echo "==> Done! wendy-agent deployed to ${HOSTNAME}"
