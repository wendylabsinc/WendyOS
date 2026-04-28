#!/bin/bash
#
# WendyOS Agent Auto-Updater Script
# Checks for and installs updates to the wendyos-agent binary
#

set -e

# Load configuration
if [ -f /etc/default/wendy-agent ]; then
    source /etc/default/wendy-agent
fi

# Default values if not configured
GITHUB_REPO="${WENDYOS_AGENT_GITHUB_REPO:-wendylabsinc/wendy-agent}"
VERSION="${WENDYOS_AGENT_VERSION:-latest}"
ARCH="aarch64"  # Hardcoded for RPi

# Paths
INSTALL_DIR="/usr/local/bin"
BACKUP_DIR="/opt/wendy/bin"
BINARY_NAME="wendy-agent"
VERSION_FILE="/var/lib/wendy-agent/current-version"

# Logging
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
    logger -t wendyos-agent-updater "$*"
}

error() {
    log "ERROR: $*" >&2
    exit 1
}

# Check network connectivity
check_network() {
    log "Checking network connectivity"

    # Try to ping GitHub
    if ! curl -s --head --connect-timeout 5 https://api.github.com >/dev/null 2>&1; then
        log "No network connectivity to GitHub, skipping update check"
        exit 0
    fi
}

# Get current installed version
get_current_version() {
    local current_version=""

    # Try to get version from the binary itself
    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        if "${INSTALL_DIR}/${BINARY_NAME}" --version >/dev/null 2>&1; then
            current_version=$("${INSTALL_DIR}/${BINARY_NAME}" --version 2>&1 | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || echo "")
        fi
    fi

    # Fall back to stored version file
    if [ -z "${current_version}" ] && [ -f "${VERSION_FILE}" ]; then
        current_version=$(cat "${VERSION_FILE}")
    fi

    echo "${current_version}"
}

# Get latest stable version from GitHub (excludes pre-releases)
get_latest_version() {
    local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

    # Get the latest stable release
    local release_info
    if command -v wget >/dev/null 2>&1; then
        release_info=$(wget -q -O - "${api_url}" 2>/dev/null) || return 1
    else
        release_info=$(curl -sL "${api_url}" 2>/dev/null) || return 1
    fi

    # Extract version tag (format: vX.Y.Z)
    local latest_version
    if command -v jq >/dev/null 2>&1; then
        latest_version=$(echo "${release_info}" | jq -r '.tag_name // empty')
    else
        latest_version=$(echo "${release_info}" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | cut -d'"' -f4)
    fi

    # Remove 'v' prefix for version comparison (v1.0.0 -> 1.0.0)
    latest_version="${latest_version#v}"

    echo "${latest_version}"
}

# Compare versions
version_gt() {
    # Returns 0 if version1 > version2
    local version1="$1"
    local version2="$2"

    if [ -z "${version1}" ] || [ -z "${version2}" ]; then
        return 1
    fi

    # Use sort -V if available
    if command -v sort >/dev/null 2>&1 && sort --help 2>&1 | grep -q -- '-V'; then
        [ "$(printf '%s\n' "${version1}" "${version2}" | sort -V | tail -1)" = "${version1}" ] && [ "${version1}" != "${version2}" ]
    else
        # Simple comparison
        [ "${version1}" != "${version2}" ]
    fi
}

# Check if update is needed
check_update() {
    local current_version=$(get_current_version)
    local latest_version=$(get_latest_version)

    if [ -z "${latest_version}" ]; then
        log "Could not determine latest version"
        return 1
    fi

    log "Current version: ${current_version:-unknown}"
    log "Latest stable version: ${latest_version}"

    if [ -z "${current_version}" ]; then
        log "No current version found, update needed"
        return 0
    fi

    if version_gt "${latest_version}" "${current_version}"; then
        log "Update available: ${current_version} -> ${latest_version}"
        return 0
    else
        log "Already up to date"
        return 1
    fi
}

# Perform update
perform_update() {
    log "Starting wendyos-agent update"

    # Stop the service if running
    if systemctl is-active --quiet wendyos-agent.service; then
        log "Stopping wendyos-agent service"
        systemctl stop wendyos-agent.service
    fi

    # Run the download script
    if /opt/wendyos/bin/download-wendyos-agent.sh; then
        log "Update completed successfully"

        # Store the new version
        mkdir -p "$(dirname "${VERSION_FILE}")"
        get_latest_version > "${VERSION_FILE}"

        # Restart the service if it was running
        if systemctl is-enabled --quiet wendyos-agent.service; then
            log "Starting wendyos-agent service"
            systemctl start wendyos-agent.service
        fi

        return 0
    else
        log "Update failed"

        # Try to restore from backup if available
        if [ -f "${BACKUP_DIR}/${BINARY_NAME}.latest" ]; then
            log "Restoring from backup"
            cp "${BACKUP_DIR}/${BINARY_NAME}.latest" "${INSTALL_DIR}/${BINARY_NAME}"
            chmod 755 "${INSTALL_DIR}/${BINARY_NAME}"
        fi

        # Restart the service if it was running
        if systemctl is-enabled --quiet wendyos-agent.service; then
            systemctl start wendyos-agent.service
        fi

        return 1
    fi
}

# Main execution
main() {
    log "Starting wendy-agent update check"

    # Check network connectivity first
    check_network

    # Check if binary exists at all
    if [ ! -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        log "Wendy-agent not installed, performing initial download"
        perform_update
        exit $?
    fi

    # Check if update is needed
    if check_update; then
        perform_update
        exit $?
    else
        log "No update needed"
        exit 0
    fi
}

# Run main function
main "$@"
