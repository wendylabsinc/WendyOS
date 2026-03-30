#!/bin/sh
# wendy-config.sh — first-boot provisioning from the WENDYCONFIG partition.
# Sources wendy-config-lib.sh for logging, stamp, and handler dispatch.

set -e

. /usr/lib/wendy-config/wendy-config-lib.sh

LABEL="WENDYCONFIG"
MOUNT_POINT="/mnt/wendyconfig"
CONF_NAME="wendy.conf"
CONF_DEST="${CONF_TMPFS}/${CONF_NAME}"

# --- Locate partition by FAT volume label ---
wc_log INFO "locating partition with label ${LABEL}"
DEV="$(findfs "LABEL=${LABEL}" 2>/dev/null)" || true
if [ -z "$DEV" ]; then
    wc_log WARN "no partition with label ${LABEL} found — nothing to provision"
    wc_stamp
    exit 0
fi
wc_log INFO "found ${LABEL} at ${DEV}"

# --- Mount read-only ---
mkdir -p "$MOUNT_POINT"
mount -o ro "$DEV" "$MOUNT_POINT"
wc_log INFO "mounted ${DEV} read-only at ${MOUNT_POINT}"

# --- Check for wendy.conf ---
if [ ! -f "${MOUNT_POINT}/${CONF_NAME}" ]; then
    wc_log WARN "no ${CONF_NAME} on ${LABEL} — nothing to provision"
    umount "$MOUNT_POINT"
    wc_stamp
    exit 0
fi

# --- Copy to tmpfs before any writes ---
mkdir -p "$CONF_TMPFS"
cp "${MOUNT_POINT}/${CONF_NAME}" "$CONF_DEST"
wc_log INFO "copied ${CONF_NAME} to ${CONF_DEST}"

# --- Remount read-write so we can wipe the original ---
mount -o remount,rw "$DEV" "$MOUNT_POINT"
wc_log INFO "remounted ${DEV} read-write"

# --- Zero-overwrite then delete ---
CONF_ON_PART="${MOUNT_POINT}/${CONF_NAME}"
BYTE_COUNT="$(stat -c %s "$CONF_ON_PART")"
dd if=/dev/zero of="$CONF_ON_PART" bs=1 count="$BYTE_COUNT" conv=notrunc 2>/dev/null
sync
rm -f "$CONF_ON_PART"
sync
wc_log INFO "wiped and removed ${CONF_NAME} from ${LABEL} (${BYTE_COUNT} bytes zeroed)"

# --- Unmount ---
umount "$MOUNT_POINT"
wc_log INFO "unmounted ${MOUNT_POINT}"

# --- Run handlers (stub until Part 3) ---
wc_run_handlers "$CONF_DEST"

# --- Write stamp ---
wc_stamp
