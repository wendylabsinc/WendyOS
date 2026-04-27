#!/bin/bash
# WendyOS mDNS UUID and Device Name Update Script
# Updates the Avahi service file with the device UUID and human-readable name

UUID_FILE="/etc/wendyos/device-uuid"
DEVICE_NAME_FILE="/etc/wendyos/device-name"
SERVICE_FILE="/etc/avahi/services/wendyos-mdns.service"

# Wait for UUID and device name files to exist (in case of race condition)
for i in {1..10}; do
    if [ -f "$UUID_FILE" ] && [ -f "$DEVICE_NAME_FILE" ]; then
        break
    fi
    sleep 1
done

if [ ! -f "$UUID_FILE" ]; then
    echo "Error: UUID file not found at $UUID_FILE"
    exit 1
fi

if [ ! -f "$DEVICE_NAME_FILE" ]; then
    echo "Warning: Device name file not found at $DEVICE_NAME_FILE, using fallback"
    # Don't exit - we'll use the fallback in the device name read step
fi

if [ ! -f "$SERVICE_FILE" ]; then
    echo "Error: Avahi service file not found at $SERVICE_FILE"
    exit 1
fi

# Read the UUID
UUID=$(cat "$UUID_FILE")

# Read the device name
DEVICE_NAME=$(cat "$DEVICE_NAME_FILE" 2>/dev/null || echo "unknown-device")

# Generate display name (Title Case with spaces)
DISPLAY_NAME=$(echo "$DEVICE_NAME" | sed 's/-/ /g' | awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) tolower(substr($i,2));}1')

# Replace placeholders in service file
sed -i "s/WENDY_DEVICE_ID/$UUID/g" "$SERVICE_FILE"
sed -i "s/WENDY_DEVICE_NAME/$DEVICE_NAME/g" "$SERVICE_FILE"
sed -i "s/WENDY_DISPLAY_NAME/$DISPLAY_NAME/g" "$SERVICE_FILE"

echo "Updated mDNS service with device UUID: $UUID"
echo "Updated mDNS service with device name: $DEVICE_NAME ($DISPLAY_NAME)"
logger -t wendyos-identity "Updated mDNS service with UUID: $UUID, name: $DEVICE_NAME"

# Reload Avahi to pick up changes if it's running
if systemctl is-active --quiet avahi-daemon; then
    avahi-daemon --reload || true
fi

exit 0
