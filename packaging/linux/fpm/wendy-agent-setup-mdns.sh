#!/usr/bin/env bash
# Populates the wendy-agent avahi service file with device-specific values.
# Called from the .deb postinstall script and can be re-run on hostname changes.
set -euo pipefail

SERVICE_FILE="/etc/avahi/services/wendy-agent.service"

if [ ! -f "$SERVICE_FILE" ]; then
  exit 0
fi

# Use device UUID if available, otherwise generate a stable one from machine-id.
if [ -f /etc/wendyos/device-uuid ]; then
  DEVICE_ID=$(cat /etc/wendyos/device-uuid)
elif [ -f /etc/machine-id ]; then
  DEVICE_ID=$(cat /etc/machine-id)
else
  DEVICE_ID=$(hostname)
fi

# Use device name if available, otherwise derive from hostname.
if [ -f /etc/wendyos/device-name ]; then
  DEVICE_NAME=$(cat /etc/wendyos/device-name)
else
  DEVICE_NAME=$(hostname)
fi

# Title-case the device name for display.
DISPLAY_NAME=$(echo "$DEVICE_NAME" | sed 's/-/ /g' | awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) tolower(substr($i,2));}1')

# Update Avahi TXT records in a way that can be safely re-run: replace the
# full key=VALUE segment regardless of the current value.
sed -i -E 's|( <txt-record>id=)[^<]*|\1'"$DEVICE_ID"'|' "$SERVICE_FILE"
sed -i -E 's|( <txt-record>name=)[^<]*|\1'"$DEVICE_NAME"'|' "$SERVICE_FILE"
sed -i -E 's|( <txt-record>displayname=)[^<]*|\1'"$DISPLAY_NAME"'|' "$SERVICE_FILE"
