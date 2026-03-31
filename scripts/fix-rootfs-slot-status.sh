#!/bin/sh
#
# fix-rootfs-slot-status.sh — Diagnose and fix unbootable rootfs slots on Jetson
#
# On NVIDIA Jetson (L4T R36.x / JetPack 6), the UEFI firmware tracks rootfs
# slot health via EFI variables:
#
#   RootfsStatusSlotA-781e084c-a330-417c-b678-38e696380cb9
#   RootfsStatusSlotB-781e084c-a330-417c-b678-38e696380cb9
#
# Format: [4-byte attributes][4-byte status]
#   attributes = 0x07 (NV + BS + RT)
#   status     = 0x00000000 (normal) or contains 0xFF (unbootable)
#
# When a Mender OTA update fails or rolls back, UEFI marks the target slot
# as unbootable (0xFF). Since nvbootctrl mark-boot-successful was removed
# in L4T 35.2.1, nothing resets this flag — the slot stays permanently
# unbootable until manually fixed via efivarfs.
#
# This script detects unbootable slots and resets them to normal.
#
# Usage:
#   fix-rootfs-slot-status.sh           # diagnose and prompt before fixing
#   fix-rootfs-slot-status.sh --fix     # fix without prompting
#   fix-rootfs-slot-status.sh --check   # diagnose only, no changes
#

set -eu

GUID="781e084c-a330-417c-b678-38e696380cb9"
EFIVAR_DIR="/sys/firmware/efi/efivars"
# 4-byte attributes (0x07 = NV+BS+RT) + 4-byte status (0x00 = normal)
NORMAL_VALUE='\x07\x00\x00\x00\x00\x00\x00\x00'

log() {
    printf '%s\n' "$*"
}

log_err() {
    printf '%s\n' "ERROR: $*" >&2
}

# Parse arguments
MODE="interactive"
case "${1:-}" in
    --fix)   MODE="fix" ;;
    --check) MODE="check" ;;
    -h|--help)
        log "Usage: $0 [--fix|--check]"
        log "  (no args)  Diagnose and prompt before fixing"
        log "  --fix      Fix unbootable slots without prompting"
        log "  --check    Diagnose only, make no changes"
        exit 0
        ;;
    "") ;;
    *)  log_err "unknown option: $1"; exit 1 ;;
esac

# Sanity checks
if [ "$(id -u)" -ne 0 ]; then
    log_err "must run as root"
    exit 1
fi

if [ ! -d "${EFIVAR_DIR}" ]; then
    log_err "efivarfs not mounted at ${EFIVAR_DIR}"
    log_err "try: mount -t efivarfs efivarfs /sys/firmware/efi/efivars"
    exit 1
fi

if ! command -v nvbootctrl >/dev/null 2>&1; then
    log_err "nvbootctrl not found -- is this a Jetson device?"
    exit 1
fi

###############################################################################
# Diagnosis
###############################################################################

log "=== Rootfs Slot Status Diagnosis ==="
log ""

# Show current boot state
current_slot=$(nvbootctrl -t rootfs get-current-slot 2>/dev/null || echo "unknown")
case "${current_slot}" in
    0) slot_label="A" ;;
    1) slot_label="B" ;;
    *) slot_label="unknown" ;;
esac
log "Current rootfs slot: ${current_slot} (${slot_label})"
log ""

# nvbootctrl view
log "--- nvbootctrl slot info ---"
nvbootctrl -t rootfs dump-slots-info 2>&1 | while IFS= read -r line; do
    log "  ${line}"
done
log ""

# Check each slot's UEFI variable
slots_to_fix=""

for slot_name in A B; do
    varfile="${EFIVAR_DIR}/RootfsStatusSlot${slot_name}-${GUID}"

    if [ ! -f "${varfile}" ]; then
        log "Slot ${slot_name}: UEFI variable MISSING (${varfile})"
        log "  -> Cannot diagnose. Variable may need to be created."
        log ""
        continue
    fi

    # Read raw bytes
    raw_hex=$(hexdump -C "${varfile}" | head -1)
    file_size=$(wc -c < "${varfile}")

    log "Slot ${slot_name}: ${varfile}"
    log "  Raw: ${raw_hex}"
    log "  Size: ${file_size} bytes"

    # Validate size (must be 8 bytes: 4 attrs + 4 data)
    if [ "${file_size}" -ne 8 ]; then
        log "  Status: CORRUPT (expected 8 bytes, got ${file_size})"
        slots_to_fix="${slots_to_fix} ${slot_name}"
        log ""
        continue
    fi

    # Read status byte (byte 4, zero-indexed) using dd
    status_byte=$(dd if="${varfile}" bs=1 skip=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')

    if [ "${status_byte}" = "ff" ]; then
        log "  Status: UNBOOTABLE (0xFF)"
        slots_to_fix="${slots_to_fix} ${slot_name}"
    elif [ "${status_byte}" = "00" ]; then
        log "  Status: normal (0x00)"
    else
        log "  Status: UNKNOWN (0x${status_byte})"
        slots_to_fix="${slots_to_fix} ${slot_name}"
    fi
    log ""
done

###############################################################################
# Fix
###############################################################################

# Trim whitespace
slots_to_fix=$(echo "${slots_to_fix}" | tr -s ' ' | sed 's/^ //')

if [ -z "${slots_to_fix}" ]; then
    log "All slots are healthy. Nothing to fix."
    exit 0
fi

log "Slots needing repair: ${slots_to_fix}"
log ""

if [ "${MODE}" = "check" ]; then
    log "Run with --fix to repair, or without flags to be prompted."
    exit 1
fi

if [ "${MODE}" = "interactive" ]; then
    printf 'Reset these slots to normal? [y/N] '
    read -r answer
    case "${answer}" in
        [yY]|[yY][eE][sS]) ;;
        *) log "Aborted."; exit 1 ;;
    esac
fi

for slot_name in ${slots_to_fix}; do
    varfile="${EFIVAR_DIR}/RootfsStatusSlot${slot_name}-${GUID}"
    log "Fixing slot ${slot_name}..."

    # Remove immutable flag (efivarfs sets this by default)
    if ! chattr -i "${varfile}" 2>/dev/null; then
        log_err "failed to remove immutable flag on ${varfile}"
        continue
    fi

    # Write normal value using dd via temp file (matches NVIDIA official pattern)
    # Format: 4-byte EFI attributes (0x07 = NV+BS+RT) + 4-byte UINT32 status (0x00 = normal)
    tmpfile=$(mktemp)
    printf "${NORMAL_VALUE}" > "${tmpfile}"
    if ! dd if="${tmpfile}" of="${varfile}" bs=8 2>/dev/null; then
        log_err "failed to write ${varfile}"
        rm -f "${tmpfile}"
        continue
    fi
    rm -f "${tmpfile}"
    sync

    # Verify the write
    new_size=$(wc -c < "${varfile}")
    new_status=$(dd if="${varfile}" bs=1 skip=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')

    if [ "${new_size}" -eq 8 ] && [ "${new_status}" = "00" ]; then
        log "  Slot ${slot_name}: fixed (verified normal)"
    else
        log_err "  Slot ${slot_name}: write succeeded but verification failed (size=${new_size}, status=0x${new_status})"
    fi
done

log ""
log "=== Post-fix verification ==="
nvbootctrl -t rootfs dump-slots-info 2>&1 | while IFS= read -r line; do
    log "  ${line}"
done
log ""

# Clean up stale marker file if present
STALE_MARKER="/data/mender/tegra-bl-version-before"
if [ -f "${STALE_MARKER}" ]; then
    rm -f "${STALE_MARKER}"
    log "Cleaned up stale marker: ${STALE_MARKER}"
fi

log "Done."
