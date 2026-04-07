#!/bin/sh
#
# system-status.sh -- Diagnose and fix Jetson boot/rootfs slot issues
#
# Provides a complete view of the Jetson boot state (system info, bootloader,
# rootfs slots, UEFI variables, capsule status, ESRT) and can fix rootfs slots
# stuck in "unbootable" (0xFF) state.
#
# On NVIDIA Jetson (L4T R36.x / JetPack 6), the UEFI firmware tracks rootfs
# slot health via EFI variables (RootfsStatusSlot{A,B}). When a Mender OTA
# update fails or rolls back, UEFI marks the target slot as unbootable (0xFF).
# Since nvbootctrl mark-boot-successful was removed in L4T 35.2.1, nothing
# resets this flag -- the slot stays permanently unbootable until fixed.
#
# Usage:
#   system-status.sh           # full diagnosis
#   system-status.sh --fix     # diagnose and fix unbootable slots
#

set -eu

NVIDIA_GUID="781e084c-a330-417c-b678-38e696380cb9"
EFI_GLOBAL_GUID="8be4df61-93ca-11d2-aa0d-00e098032b8c"
EFIVAR_DIR="/sys/firmware/efi/efivars"
NORMAL_VALUE='\x07\x00\x00\x00\x00\x00\x00\x00'

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log() {
    printf '%s\n' "$*"
}

log_err() {
    printf "ERROR: ${RED}%s${NC}\n" "$*" >&2
}

# Parse arguments
MODE="check"
case "${1:-}" in
    --fix) MODE="fix" ;;
    -h|--help)
        log "Usage: $0 [--fix]"
        log "  (no args)  Full diagnosis, no changes"
        log "  --fix      Diagnose and fix unbootable slots"
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

# Helper: color the status portion of nvbootctrl output
print_slot_line() {
    line="$1"
    case "${line}" in
        *"status: normal"*)
            printf "  %sstatus: ${GREEN}normal${NC}\n" "$(echo "${line}" | sed 's/status: normal$//')"
            ;;
        *"status: unbootable"*)
            printf "  %sstatus: ${RED}unbootable${NC}\n" "$(echo "${line}" | sed 's/status: unbootable$//')"
            ;;
        *"retry_count: 0,"*)
            printf "  %s${RED}retry_count: 0${NC},%s\n" \
                "$(echo "${line}" | sed 's/retry_count: 0,.*//')" \
                "$(echo "${line}" | sed 's/.*retry_count: 0,//')"
            ;;
        *)
            log "  ${line}"
            ;;
    esac
}

###############################################################################
# System Information
###############################################################################

log "=== System Info ==="
log "Date: $(date)"
log "Kernel: $(uname -r)"
if [ -f /etc/os-release ]; then
    . /etc/os-release
    printf "OS: %s ${BLUE}%s${NC}\n" "${NAME:-unknown}" "${VERSION:-}"
fi
log "Root: $(findmnt -no SOURCE,FSTYPE,OPTIONS / 2>/dev/null || echo 'unknown')"
log ""

###############################################################################
# Bootloader and Rootfs Slots
###############################################################################

current_slot=$(nvbootctrl -t rootfs get-current-slot 2>/dev/null || echo "unknown")
case "${current_slot}" in
    0) slot_label="A" ;;
    1) slot_label="B" ;;
    *) slot_label="unknown" ;;
esac

# Bootloader slot (from non-rootfs nvbootctrl)
bl_slot=$(nvbootctrl get-current-slot 2>/dev/null || echo "unknown")
case "${bl_slot}" in
    0) bl_label="A" ;;
    1) bl_label="B" ;;
    *) bl_label="unknown" ;;
esac

# Rootfs A/B slot count
rootfs_num_slots=$(nvbootctrl -t rootfs get-number-slots 2>/dev/null || echo "unknown")

log "=== Boot Slots ==="

log "--- Bootloader ---"
nvbootctrl dump-slots-info 2>&1 | while IFS= read -r line; do
    print_slot_line "${line}"
done
log ""

log "--- Rootfs ---"
nvbootctrl -t rootfs dump-slots-info 2>&1 | while IFS= read -r line; do
    print_slot_line "${line}"
done
log ""

# Check: rootfs A/B enabled?
if [ "${rootfs_num_slots}" != "2" ]; then
    printf "Rootfs A/B: ${RED}NOT ENABLED (${rootfs_num_slots} slot(s))${NC}\n"
    printf "  ${YELLOW}Mender OTA requires 2 rootfs slots. This device needs a reflash.${NC}\n"
    if [ -f /etc/nv_boot_control.conf ]; then
        tnspec=$(grep '^TNSPEC' /etc/nv_boot_control.conf | head -1)
        log "  ${tnspec}"
    fi
else
    printf "Rootfs A/B: ${GREEN}enabled (2 slots)${NC}\n"
fi

# Check: bootloader chain matches rootfs slot?
# On Jetson, chain A = rootfs A, chain B = rootfs B
if [ "${bl_label}" != "unknown" ] && [ "${slot_label}" != "unknown" ]; then
    if [ "${bl_label}" = "${slot_label}" ]; then
        printf "BL/rootfs sync: ${GREEN}OK${NC} (both on slot ${slot_label})\n"
    else
        printf "BL/rootfs sync: ${RED}MISMATCH -- bootloader on ${bl_label}, rootfs on ${slot_label}${NC}\n"
        printf "  ${YELLOW}Bootloader chain and rootfs slot should always match on Jetson.${NC}\n"
    fi
fi

# Check: do both rootfs partitions exist on disk?
app_a=$(readlink -f /dev/disk/by-partlabel/APP_a 2>/dev/null || echo "")
app_b=$(readlink -f /dev/disk/by-partlabel/APP_b 2>/dev/null || echo "")
if [ -n "${app_a}" ] && [ -n "${app_b}" ]; then
    printf "Rootfs partitions: ${GREEN}OK${NC} (APP_a=${app_a}, APP_b=${app_b})\n"
else
    if [ -z "${app_a}" ]; then
        printf "Rootfs partition APP_a: ${RED}MISSING${NC}\n"
    fi
    if [ -z "${app_b}" ]; then
        printf "Rootfs partition APP_b: ${RED}MISSING${NC}\n"
    fi
    printf "  ${YELLOW}Device may need a reflash to create the partition layout.${NC}\n"
fi

# Show nv_boot_control.conf TNSPEC for reference
if [ -f /etc/nv_boot_control.conf ]; then
    tnspec=$(grep '^TNSPEC' /etc/nv_boot_control.conf | head -1)
    log "TNSPEC: ${tnspec#TNSPEC }"
fi
log ""

###############################################################################
# UEFI RootfsStatusSlot Variables
###############################################################################

log "=== UEFI RootfsStatusSlot Variables ==="

slots_to_fix=""

for slot_name in A B; do
    varfile="${EFIVAR_DIR}/RootfsStatusSlot${slot_name}-${NVIDIA_GUID}"

    if [ ! -f "${varfile}" ]; then
        printf "Slot ${slot_name}: ${RED}UEFI variable MISSING${NC}\n"
        log ""
        continue
    fi

    raw_hex=$(hexdump -C "${varfile}" | head -1)
    file_size=$(wc -c < "${varfile}")

    if [ "${file_size}" -ne 8 ]; then
        printf "Slot ${slot_name}: ${RED}CORRUPT (expected 8 bytes)${NC}\n"
        log "  ${raw_hex}"
        slots_to_fix="${slots_to_fix} ${slot_name}"
        continue
    fi

    status_byte=$(dd if="${varfile}" bs=1 skip=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
    case "${status_byte}" in
        00)
            printf "Slot ${slot_name}: ${GREEN}normal${NC}\n"
            ;;
        ff)
            printf "Slot ${slot_name}: ${RED}UNBOOTABLE (0xFF)${NC}\n"
            slots_to_fix="${slots_to_fix} ${slot_name}"
            ;;
        *)
            printf "Slot ${slot_name}: ${RED}UNKNOWN (0x${status_byte})${NC}\n"
            slots_to_fix="${slots_to_fix} ${slot_name}"
            ;;
    esac
    log "  ${raw_hex}"
done
log ""

###############################################################################
# Mender Root Device Check
###############################################################################

log "=== Mender Root Device ==="

# What is actually mounted as root?
actual_root=$(findmnt -no SOURCE / 2>/dev/null || echo "unknown")
log "Mounted root: ${actual_root}"

# Map slot to expected root device via mender.conf
# Mender uses split-config: RootfsPartA/B are in the persistent config
# at /data/mender/mender.conf, not in /etc/mender/mender.conf (transient).
expected_root=""
rootfs_a=""
rootfs_b=""
for conf in /data/mender/mender.conf /var/lib/mender/mender.conf /etc/mender/mender.conf; do
    [ -f "${conf}" ] || continue
    if [ -z "${rootfs_a}" ]; then
        rootfs_a=$(sed -n 's/.*"RootfsPartA"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${conf}" | head -1)
    fi
    if [ -z "${rootfs_b}" ]; then
        rootfs_b=$(sed -n 's/.*"RootfsPartB"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${conf}" | head -1)
    fi
done
log "Mender config: RootfsPartA=${rootfs_a:-not set}  RootfsPartB=${rootfs_b:-not set}"

case "${current_slot}" in
    0) expected_root="${rootfs_a}" ;;
    1) expected_root="${rootfs_b}" ;;
esac

# Check for upgrade_available flag (indicates uncommitted update)
upgrade_marker="/var/lib/mender/upgrade_available"
if [ -f "${upgrade_marker}" ]; then
    printf "Upgrade pending: ${YELLOW}yes (uncommitted update)${NC}\n"
else
    printf "Upgrade pending: ${GREEN}no${NC}\n"
fi

# Cross-reference: does the mounted root match what nvbootctrl + mender.conf expect?
root_mismatch=0
if [ -n "${expected_root}" ] && [ "${actual_root}" != "unknown" ]; then
    if [ "${actual_root}" = "${expected_root}" ]; then
        printf "Root device check: ${GREEN}OK${NC} (slot ${slot_label} = ${expected_root})\n"
    else
        root_mismatch=1
        printf "Root device check: ${RED}MISMATCH${NC}\n"
        printf "  Expected: ${expected_root} (slot ${slot_label} per mender.conf)\n"
        printf "  Actual:   ${actual_root}\n"
        printf "  ${YELLOW}UEFI likely fell back to the wrong slot due to an unbootable target.${NC}\n"
        printf "  ${YELLOW}Fix: run '$0 --fix' then reboot.${NC}\n"
    fi
else
    log "Root device check: could not verify (mender.conf missing or slot unknown)"
fi
log ""

###############################################################################
# Capsule and ESRT Status
###############################################################################

log "=== UEFI Capsule Status ==="

# OsIndications
osind_file="${EFIVAR_DIR}/OsIndications-${EFI_GLOBAL_GUID}"
if [ -f "${osind_file}" ]; then
    osind_hex=$(hexdump -C "${osind_file}" | head -1)
    osind_byte=$(dd if="${osind_file}" bs=1 skip=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
    if [ "${osind_byte}" = "04" ] || [ "${osind_byte}" = "06" ] || [ "${osind_byte}" = "07" ]; then
        printf "OsIndications: ${YELLOW}capsule pending (0x${osind_byte})${NC}\n"
    else
        log "OsIndications: 0x${osind_byte}"
    fi
    log "  ${osind_hex}"
else
    printf "OsIndications: ${GREEN}not set${NC}\n"
fi

# Capsule on ESP
if [ -d /boot/efi/EFI/UpdateCapsule ]; then
    capsule_count=$(find /boot/efi/EFI/UpdateCapsule -maxdepth 1 -type f 2>/dev/null | wc -l)
    if [ "${capsule_count}" -gt 0 ]; then
        printf "Capsule staged: ${YELLOW}yes${NC}\n"
        ls -lh /boot/efi/EFI/UpdateCapsule/ 2>/dev/null | while IFS= read -r line; do
            log "  ${line}"
        done
    else
        printf "Capsule staged: ${GREEN}no${NC}\n"
    fi
else
    printf "Capsule staged: ${GREEN}no${NC}\n"
fi

# Capsule on rootfs
if [ -f /opt/nvidia/UpdateCapsule/tegra-bl.cap ]; then
    cap_size=$(ls -lh /opt/nvidia/UpdateCapsule/tegra-bl.cap | awk '{print $5}')
    log "Capsule on rootfs: ${cap_size}"
else
    log "Capsule on rootfs: not found"
fi
log ""

# ESRT
log "=== ESRT (EFI System Resource Table) ==="
if [ -d /sys/firmware/efi/esrt/entries ]; then
    for entry in /sys/firmware/efi/esrt/entries/entry*; do
        [ -d "${entry}" ] || continue
        last_status=$(cat "${entry}/last_attempt_status" 2>/dev/null || echo "N/A")
        log "$(basename "${entry}"):"
        log "  FW Version:   $(cat "${entry}/fw_version" 2>/dev/null || echo 'N/A')"
        log "  FW Type:      $(cat "${entry}/fw_type" 2>/dev/null || echo 'N/A')"
        log "  FW Class:     $(cat "${entry}/fw_class" 2>/dev/null || echo 'N/A')"
        if [ "${last_status}" = "0" ]; then
            printf "  Last Status:  ${GREEN}%s (success)${NC}\n" "${last_status}"
        elif [ "${last_status}" = "N/A" ]; then
            log "  Last Status:  N/A"
        else
            printf "  Last Status:  ${RED}%s (error)${NC}\n" "${last_status}"
        fi
        log "  Last Version: $(cat "${entry}/last_attempt_version" 2>/dev/null || echo 'N/A')"
    done
else
    log "ESRT not available"
fi
log ""

###############################################################################
# Mender Marker Files
###############################################################################

log "=== Mender Markers ==="

if [ -f /var/lib/wendyos/update-bootloader ]; then
    printf "Bootloader update marker: ${YELLOW}PRESENT${NC}\n"
else
    printf "Bootloader update marker: ${GREEN}absent (clean)${NC}\n"
fi

if [ -f /data/mender/tegra-bl-version-before ]; then
    ver=$(cat /data/mender/tegra-bl-version-before 2>/dev/null || echo "unreadable")
    printf "BL version-before file: ${YELLOW}PRESENT (${ver})${NC}\n"
else
    printf "BL version-before file: ${GREEN}absent (clean)${NC}\n"
fi
log ""

###############################################################################
# Disk Usage
###############################################################################

log "=== Disk Usage ==="
df -h / /boot/efi /data 2>/dev/null | while IFS= read -r line; do
    log "  ${line}"
done
log ""

###############################################################################
# --fix: Fix unbootable rootfs slots
###############################################################################

slots_to_fix=$(echo "${slots_to_fix}" | tr -s ' ' | sed 's/^ //')

if [ -z "${slots_to_fix}" ]; then
    if [ "${root_mismatch}" -eq 1 ]; then
        printf "All UEFI slot variables are healthy, but ${RED}root device mismatch detected${NC}.\n"
        log "This likely means UEFI fell back after a previous slot was marked unbootable,"
        log "and the slot has since been repaired. Reboot to let UEFI boot the correct slot."
    else
        printf "All rootfs slots are ${GREEN}healthy${NC}.\n"
    fi
    exit 0
fi

printf "Slots needing repair: ${RED}${slots_to_fix}${NC}\n"
log ""

if [ "${MODE}" = "check" ]; then
    log "Run with --fix to repair."
    exit 1
fi

for slot_name in ${slots_to_fix}; do
    varfile="${EFIVAR_DIR}/RootfsStatusSlot${slot_name}-${NVIDIA_GUID}"
    log "Fixing slot ${slot_name}..."

    if ! chattr -i "${varfile}" 2>/dev/null; then
        log_err "failed to remove immutable flag on ${varfile}"
        continue
    fi

    tmpfile=$(mktemp)
    printf "${NORMAL_VALUE}" > "${tmpfile}"
    if ! dd if="${tmpfile}" of="${varfile}" bs=8 count=1 2>/dev/null; then
        log_err "failed to write ${varfile}"
        rm -f "${tmpfile}"
        continue
    fi
    rm -f "${tmpfile}"
    sync

    new_size=$(wc -c < "${varfile}")
    new_status=$(dd if="${varfile}" bs=1 skip=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')

    if [ "${new_size}" -eq 8 ] && [ "${new_status}" = "00" ]; then
        printf "  Slot ${slot_name}: ${GREEN}fixed (verified normal)${NC}\n"
    else
        log_err "Slot ${slot_name}: verification failed (size=${new_size}, status=0x${new_status})"
    fi
done

log ""

# Clean up stale marker
STALE_MARKER="/data/mender/tegra-bl-version-before"
if [ -f "${STALE_MARKER}" ]; then
    rm -f "${STALE_MARKER}"
    printf "Stale marker: ${GREEN}cleaned${NC} (${STALE_MARKER})\n"
fi

if [ "${root_mismatch}" -eq 1 ]; then
    log ""
    printf "${YELLOW}Root device mismatch detected. Reboot after fix to let UEFI boot the correct slot.${NC}\n"
fi

log ""
log "=== Post-fix Slot Status ==="
log "--- Bootloader ---"
nvbootctrl dump-slots-info 2>&1 | while IFS= read -r line; do
    print_slot_line "${line}"
done
log ""
log "--- Rootfs ---"
nvbootctrl -t rootfs dump-slots-info 2>&1 | while IFS= read -r line; do
    print_slot_line "${line}"
done
log ""
log "Done."
