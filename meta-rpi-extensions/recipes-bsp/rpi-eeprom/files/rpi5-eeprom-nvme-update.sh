#!/bin/bash
#
# Raspberry Pi 5 NVMe EEPROM Configuration Script
# Updates PCIE_PROBE and BOOT_ORDER for NVMe boot support
#

set -e

LOGFILE="/var/log/rpi5-eeprom-nvme-update.log"
FLAGFILE="/var/lib/wendyos/eeprom-nvme-updated"

# Function to log messages
log_message() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "${LOGFILE}"
    logger -t "rpi5-eeprom-nvme-update" "$1"
}

# Function to detect Raspberry Pi model
detect_pi_model() {
    if [ ! -f /proc/device-tree/model ]; then
        return 1
    fi

    MODEL=$(tr -d '\0' < /proc/device-tree/model)
    if [[ "${MODEL}" == *"Raspberry Pi 5"* ]]; then
        return 0
    fi
    return 1
}

# Main execution
main() {
    log_message "Starting Raspberry Pi 5 NVMe EEPROM configuration check"

    # Check if already updated
    if [ -f "${FLAGFILE}" ]; then
        log_message "NVMe EEPROM already configured (flag file exists)"
        exit 0
    fi

    # Detect if this is a Raspberry Pi 5
    if ! detect_pi_model; then
        log_message "Not a Raspberry Pi 5, skipping NVMe EEPROM configuration"
        mkdir -p "$(dirname "${FLAGFILE}")"
        touch "${FLAGFILE}"
        exit 0
    fi

    log_message "Raspberry Pi 5 detected, checking NVMe EEPROM configuration"

    # Check if rpi-eeprom-config is available
    if ! command -v rpi-eeprom-config &> /dev/null; then
        log_message "ERROR: rpi-eeprom-config not found. Please install rpi-eeprom package"
        exit 1
    fi

    # Get current EEPROM configuration
    CURRENT_CONFIG=$(rpi-eeprom-config 2>/dev/null || true)
    if [ -z "${CURRENT_CONFIG}" ]; then
        log_message "ERROR: Failed to read current EEPROM configuration"
        exit 1
    fi

    NEEDS_UPDATE=0

    # Extract current PCIE_PROBE value
    CURRENT_PCIE_PROBE=$(echo "${CURRENT_CONFIG}" | grep "^PCIE_PROBE=" | cut -d'=' -f2 || echo "")

    if [ -z "${CURRENT_PCIE_PROBE}" ]; then
        log_message "PCIE_PROBE not found in current configuration, will add it"
        NEEDS_UPDATE=1
    elif [ "${CURRENT_PCIE_PROBE}" != "1" ]; then
        log_message "Current PCIE_PROBE=${CURRENT_PCIE_PROBE}, needs update to 1"
        NEEDS_UPDATE=1
    else
        log_message "PCIE_PROBE already set to 1, no update needed"
    fi

    # Extract current BOOT_ORDER value
    CURRENT_BOOT_ORDER=$(echo "${CURRENT_CONFIG}" | grep "^BOOT_ORDER=" | cut -d'=' -f2 || echo "")

    if [ -z "${CURRENT_BOOT_ORDER}" ]; then
        log_message "BOOT_ORDER not found in current configuration, will add it"
        NEEDS_UPDATE=1
    elif [ "${CURRENT_BOOT_ORDER}" != "0xf416" ]; then
        log_message "Current BOOT_ORDER=${CURRENT_BOOT_ORDER}, needs update to 0xf416"
        NEEDS_UPDATE=1
    else
        log_message "BOOT_ORDER already set to 0xf416, no update needed"
    fi

    if [ ${NEEDS_UPDATE} -eq 1 ]; then
        log_message "Updating NVMe EEPROM configuration..."

        # Create temporary directory for EEPROM files
        # (TMPDIR is a reserved POSIX variable used by mktemp; use a different name)
        WORK_TMPDIR=$(mktemp -d)
        trap 'rm -rf "${WORK_TMPDIR}"' EXIT

        # Find the RPi5 EEPROM binary - check multiple locations
        FIRMWARE_DIR="/lib/firmware/raspberrypi/bootloader-2712"
        FIRMWARE_PATH=""

        # Check default directory first
        if [ -d "${FIRMWARE_DIR}/default" ]; then
            FIRMWARE_PATH=$(find "${FIRMWARE_DIR}/default" -name 'pieeprom-*.bin' -print -quit)
        fi

        # If not found, check stable directory
        if [ -z "$FIRMWARE_PATH" ] && [ -d "${FIRMWARE_DIR}/stable" ]; then
            FIRMWARE_PATH=$(find "${FIRMWARE_DIR}/stable" -name 'pieeprom-*.bin' -print -quit)
        fi

        # If still not found, check latest directory
        if [ -z "$FIRMWARE_PATH" ] && [ -d "${FIRMWARE_DIR}/latest" ]; then
            FIRMWARE_PATH=$(find "${FIRMWARE_DIR}/latest" -name 'pieeprom-*.bin' -print -quit)
        fi

        if [ -z "${FIRMWARE_PATH}" ] || [ ! -f "${FIRMWARE_PATH}" ]; then
            log_message "ERROR: Could not find RPi5 EEPROM firmware binary"
            exit 1
        fi

        log_message "Using bootloader image: ${FIRMWARE_PATH}"

        # Extract current config to temporary file
        if ! rpi-eeprom-config "${FIRMWARE_PATH}" --out "${WORK_TMPDIR}/bootconf.txt"; then
            log_message "ERROR: Failed to extract EEPROM config"
            exit 1
        fi

        # Update or add PCIE_PROBE setting (required for non-HAT+ PCIe adapters)
        sed -i '/^PCIE_PROBE=/d' "${WORK_TMPDIR}/bootconf.txt"
        echo "PCIE_PROBE=1" >> "${WORK_TMPDIR}/bootconf.txt"

        # Update or add BOOT_ORDER setting (SD=1, NVMe=6, Network=4, restart=f)
        sed -i '/^BOOT_ORDER=/d' "${WORK_TMPDIR}/bootconf.txt"
        echo "BOOT_ORDER=0xf416" >> "${WORK_TMPDIR}/bootconf.txt"

        # Create new EEPROM image with updated config
        if ! rpi-eeprom-config "${FIRMWARE_PATH}" --config "${WORK_TMPDIR}/bootconf.txt" --out "${WORK_TMPDIR}/pieeprom-new.bin"; then
            log_message "ERROR: Failed to create new EEPROM binary"
            exit 1
        fi

        if [ ! -f "${WORK_TMPDIR}/pieeprom-new.bin" ]; then
            log_message "ERROR: Failed to create new EEPROM image"
            exit 1
        fi

        # Stage the update for next boot
        log_message "Staging NVMe EEPROM update for next boot..."
        if ! rpi-eeprom-update -d -f "${WORK_TMPDIR}/pieeprom-new.bin"; then
            log_message "ERROR: Failed to stage EEPROM update"
            exit 1
        fi

        log_message "NVMe EEPROM update staged successfully. Rebooting to apply changes..."

        # Create flag file to prevent running again after reboot
        mkdir -p "$(dirname "${FLAGFILE}")"
        touch "${FLAGFILE}"

        # Sync filesystem before reboot
        sync

        # Give time for logs to be written
        sleep 2

        # Reboot the system to apply EEPROM update
        log_message "Initiating system reboot..."
        reboot
    else
        # Create flag file since no update needed
        mkdir -p "$(dirname "${FLAGFILE}")"
        touch "${FLAGFILE}"
    fi

    log_message "NVMe EEPROM configuration check completed"
}

# Run main function
main "$@"
