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

# Enable PipeWire user services so /dev/video0 can be shared across multiple
# simultaneous readers. Skip silently when pipewire is not installed.
if command -v pipewire >/dev/null 2>&1 && command -v wireplumber >/dev/null 2>&1; then
  # Find a non-root human user with a home directory to run PipeWire under.
  # Prefer a "wendy" user if present; otherwise take the first UID >= 1000.
  PIPEWIRE_USER=""
  if id -u wendy >/dev/null 2>&1; then
    PIPEWIRE_USER="wendy"
  else
    PIPEWIRE_USER=$(getent passwd | awk -F: '$3 >= 1000 && $3 < 65534 && $6 != "" { print $1; exit }')
  fi

  if [ -n "$PIPEWIRE_USER" ]; then
    loginctl enable-linger "$PIPEWIRE_USER" >/dev/null 2>&1 || true
    PIPEWIRE_UID=$(id -u "$PIPEWIRE_USER")
    # XDG_RUNTIME_DIR must be set for systemctl --user to target the right session.
    export XDG_RUNTIME_DIR="/run/user/$PIPEWIRE_UID"
    su - "$PIPEWIRE_USER" -c "
      export XDG_RUNTIME_DIR=/run/user/\$(id -u)
      systemctl --user enable pipewire wireplumber >/dev/null 2>&1 || true
      systemctl --user start pipewire wireplumber >/dev/null 2>&1 || true
    " >/dev/null 2>&1 || true
  fi
fi
