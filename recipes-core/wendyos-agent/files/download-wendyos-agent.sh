#!/bin/bash
#
# WendyOS Agent Download Script
# Downloads the wendy-agent binary from GitHub releases
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
INSTALL_DIR="/usr/bin"
BACKUP_DIR="/opt/wendy/bin"
BINARY_NAME="wendy-agent"
TEMP_DIR="/tmp/wendy-agent-download-$$"

# Logging
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
    logger -t wendy-agent-download "$*"
}

error() {
    log "ERROR: $*" >&2
    exit 1
}

# Cleanup on exit
cleanup() {
    rm -rf "${TEMP_DIR}"
}
trap cleanup EXIT

# Check for required tools
check_requirements() {
    local missing_tools=()

    for tool in curl jq tar gzip; do
        if ! command -v $tool >/dev/null 2>&1; then
            missing_tools+=($tool)
        fi
    done

    if [ ${#missing_tools[@]} -gt 0 ]; then
        log "Installing missing tools: ${missing_tools[*]}"
        if command -v apt-get >/dev/null 2>&1; then
            apt-get update && apt-get install -y ${missing_tools[*]}
        elif command -v opkg >/dev/null 2>&1; then
            opkg update && opkg install ${missing_tools[*]}
        else
            error "Missing required tools: ${missing_tools[*]}"
        fi
    fi
}

# Get release information from GitHub
# Fetches latest stable release (excludes pre-releases)
get_release_info() {
    local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

    log "Fetching latest stable release from GitHub (excludes pre-releases)..." >&2

    # Use wget if available, otherwise curl
    local release_json
    if command -v wget >/dev/null 2>&1; then
        release_json=$(wget -q -O - "${api_url}" 2>/dev/null) || error "Failed to fetch latest release"
    else
        release_json=$(curl -sL "${api_url}" 2>/dev/null) || error "Failed to fetch latest release"
    fi

    # Validate we got a proper release
    if command -v jq >/dev/null 2>&1; then
        local tag_name=$(echo "${release_json}" | jq -r '.tag_name // empty')
        if [ -z "${tag_name}" ]; then
            error "No stable releases found. Please create a release with semver tag (e.g., v1.0.0)"
        fi
    fi

    echo "${release_json}"
}

# Download the binary - simplified version
download_binary() {
    local release_info="$1"

    # Create temp directory
    mkdir -p "${TEMP_DIR}"

    # Extract download URL for aarch64 binary
    # Expected asset name format (guaranteed by workflow):
    #   wendy-agent-linux-static-musl-aarch64-vX.Y.Z.tar.gz
    # Example: wendy-agent-linux-static-musl-aarch64-v0.2.0.tar.gz
    local download_url

    if command -v jq >/dev/null 2>&1; then
        # Use jq if available - match wendy-agent platform archive (not wendy-cli)
        download_url=$(echo "${release_info}" | jq -r '.assets[]? | select(.name | test("wendy-agent-linux-arm64.*\\.tar\\.gz$")) | .browser_download_url' 2>/dev/null | head -1)
    else
        # Fallback to grep - look for the URL pattern
        download_url=$(echo "${release_info}" | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*wendy-agent-linux-arm64[^"]*\.tar\.gz[^"]*"' | head -1 | cut -d'"' -f4)
    fi

    if [ -z "${download_url}" ] || [ "${download_url}" = "null" ]; then
        error "No suitable binary found for architecture: ${ARCH} in ${release_info} ${download_url}"
    fi

    local filename="wendy-agent.tar.gz"
    log "Downloading: ${download_url}"

    # Use wget if available, otherwise curl
    if command -v wget >/dev/null 2>&1; then
        wget -O "${TEMP_DIR}/${filename}" "${download_url}" || error "Download failed"
    else
        curl -L -o "${TEMP_DIR}/${filename}" "${download_url}" || error "Download failed"
    fi

    # Extract the archive
    log "Extracting archive"
    tar -xzf "${TEMP_DIR}/${filename}" -C "${TEMP_DIR}"

    # Find the binary (exclude wendy-cli)
    local binary_path
    binary_path=$(find "${TEMP_DIR}" -name "wendy-agent" -type f ! -path "*/wendy-cli*" | head -1)

    if [ -z "${binary_path}" ]; then
        error "Binary not found in archive"
    fi

    mv "${binary_path}" "${TEMP_DIR}/${BINARY_NAME}"
    chmod +x "${TEMP_DIR}/${BINARY_NAME}"

    log "Binary downloaded and prepared successfully"
}

# Install the binary
install_binary() {
    # Create directories if they don't exist
    mkdir -p "${INSTALL_DIR}"
    mkdir -p "${BACKUP_DIR}"
    mkdir -p "/var/lib/wendy-agent"

    log "Created required directories"

    # Backup existing binary if it exists
    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        log "Backing up existing binary"
        cp "${INSTALL_DIR}/${BINARY_NAME}" "${BACKUP_DIR}/${BINARY_NAME}.backup.$(date +%Y%m%d_%H%M%S)"
    fi

    # Install new binary
    log "Installing new binary to ${INSTALL_DIR}/${BINARY_NAME}"
    mv "${TEMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

    # Ensure proper permissions
    chmod 755 "${INSTALL_DIR}/${BINARY_NAME}"

    # Keep a backup copy
    cp "${INSTALL_DIR}/${BINARY_NAME}" "${BACKUP_DIR}/${BINARY_NAME}.latest"

    log "wendyos-agent installed successfully"
}

# Main execution
main() {
    log "Starting wendy-agent download"
    log "Configuration: REPO=${GITHUB_REPO}, VERSION=${VERSION}, ARCH=${ARCH}"

    check_requirements

    local release_info
    release_info=$(get_release_info)

    download_binary "${release_info}"
    install_binary

    # Get version info if possible
    if "${INSTALL_DIR}/${BINARY_NAME}" --version >/dev/null 2>&1; then
        local installed_version=$("${INSTALL_DIR}/${BINARY_NAME}" --version 2>&1 | head -1)
        log "Installed version: ${installed_version}"
    fi

    log "Wendy-agent download completed successfully"
}

# Run main function
main "$@"
