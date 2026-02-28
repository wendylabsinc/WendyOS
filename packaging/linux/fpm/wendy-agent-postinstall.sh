#!/usr/bin/env bash
set -euo pipefail

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  exit 0
fi

systemctl daemon-reload >/dev/null 2>&1 || true

if systemctl is-enabled wendy-agent >/dev/null 2>&1; then
  systemctl try-restart wendy-agent >/dev/null 2>&1 || true
else
  systemctl enable --now wendy-agent >/dev/null 2>&1 || true
fi

# Enable the dev registry import service (runs once on first boot)
systemctl enable wendyos-dev-registry-import >/dev/null 2>&1 || true
# Start it now if the import hasn't been done yet
systemctl start wendyos-dev-registry-import >/dev/null 2>&1 || true

# Reload avahi-daemon so it picks up the new service file
systemctl try-restart avahi-daemon >/dev/null 2>&1 || true
