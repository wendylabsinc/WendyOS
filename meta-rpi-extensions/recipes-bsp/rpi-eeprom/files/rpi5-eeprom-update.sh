#!/bin/bash
#
# Raspberry Pi 5 EEPROM Power Configuration Script
# Updates PSU_MAX_CURRENT to 3000mA for proper USB gadget operation
#

set -e

LOGFILE="/var/log/rpi5-eeprom-update.log"
FLAGFILE="/var/lib/wendyos/eeprom-updated"

# Function to log messages
log_message() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "${LOGFILE}"
    logger -t "rpi5-eeprom-update" "$1"
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
    log_message "Starting Raspberry Pi 5 EEPROM configuration check"

    # Check if already updated
    if [ -f "${FLAGFILE}" ]; then
        log_message "EEPROM already configured (flag file exists)"
        exit 0
    fi

    # Detect if this is a Raspberry Pi 5
    if ! detect_pi_model; then
        log_message "Not a Raspberry Pi 5, skipping EEPROM configuration"
        mkdir -p "$(dirname "${FLAGFILE}")"
        touch "${FLAGFILE}"
        exit 0
    fi

    log_message "Raspberry Pi 5 detected, checking EEPROM configuration"

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

    # Extract current PSU_MAX_CURRENT value
    CURRENT_PSU_CURRENT=$(echo "${CURRENT_CONFIG}" | grep "^PSU_MAX_CURRENT=" | cut -d'=' -f2 || echo "")

    if [ -z "${CURRENT_PSU_CURRENT}" ]; then
        log_message "PSU_MAX_CURRENT not found in current configuration, will add it"
        NEEDS_UPDATE=1
    elif [ "${CURRENT_PSU_CURRENT}" != "3000" ]; then
        log_message "Current PSU_MAX_CURRENT=${CURRENT_PSU_CURRENT}, needs update to 3000"
        NEEDS_UPDATE=1
    else
        log_message "PSU_MAX_CURRENT already set to 3000, no update needed"
        NEEDS_UPDATE=0
    fi

    if [ ${NEEDS_UPDATE} -eq 1 ]; then
        log_message "Updating EEPROM configuration..."

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

        # Update or add PSU_MAX_CURRENT setting
        sed -i '/^PSU_MAX_CURRENT=/d' "${WORK_TMPDIR}/bootconf.txt"
        echo "PSU_MAX_CURRENT=3000" >> "${WORK_TMPDIR}/bootconf.txt"

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
        log_message "Staging EEPROM update for next boot..."
        if ! rpi-eeprom-update -d -f "${WORK_TMPDIR}/pieeprom-new.bin"; then
            log_message "ERROR: Failed to stage EEPROM update"
            exit 1
        fi

        log_message "EEPROM update staged successfully. Rebooting to apply changes..."

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

    log_message "EEPROM configuration check completed"
}

# Run main function
main "$@"
