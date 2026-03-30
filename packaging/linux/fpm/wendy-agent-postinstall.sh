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

# Stop and disable legacy dev registry services if present (registry is now embedded in the agent)
systemctl stop wendyos-dev-registry >/dev/null 2>&1 || true
systemctl disable wendyos-dev-registry >/dev/null 2>&1 || true
systemctl stop wendyos-dev-registry-import >/dev/null 2>&1 || true
systemctl disable wendyos-dev-registry-import >/dev/null 2>&1 || true

# Populate avahi service TXT records with device-specific values
if [ -x /usr/lib/wendy-agent/setup-mdns.sh ]; then
  if ! /usr/lib/wendy-agent/setup-mdns.sh; then
    echo "warning: /usr/lib/wendy-agent/setup-mdns.sh failed; mDNS TXT records may not be updated" >&2
  fi
fi

# Reload avahi-daemon so it picks up the new service file
systemctl try-restart avahi-daemon >/dev/null 2>&1 || true
