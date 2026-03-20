#!/bin/bash
#
# WendyOS CUDA Environment Detection Script
# Automatically detects CUDA version and creates environment file
#

set -Eeuo pipefail

ENV_FILE="/etc/default/wendyos-cuda"

# Logging (output to stderr so it doesn't pollute function return values)
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
    logger -t wendyos-cuda "$*" || true
}

# Detect CUDA version
detect_cuda_version() {
    local cuda_ver=""

    # Try to detect from symlink
    if [ -L /usr/local/cuda ]; then
        local cuda_path=$(readlink -f /usr/local/cuda)
        cuda_ver=$(basename "$cuda_path" | sed 's/cuda-//')
        log "Detected CUDA version from symlink: $cuda_ver"
    fi

    # Fallback: search for cuda-* directories
    if [ -z "$cuda_ver" ]; then
        for dir in /usr/local/cuda-*; do
            if [ -d "$dir" ]; then
                cuda_ver=$(basename "$dir" | sed 's/cuda-//')
                log "Detected CUDA version from directory: $cuda_ver"
                break
            fi
        done
    fi

    echo "$cuda_ver"
}

main() {
    log "Starting CUDA environment detection"

    local cuda_ver=$(detect_cuda_version)

    if [ -z "$cuda_ver" ]; then
        log "WARNING: CUDA installation not found"
        exit 0
    fi

    log "Generating CUDA environment file for version $cuda_ver"

    # Generate environment file
    # Note: Yocto builds use /usr/local/cuda-X.X/lib/ (not lib64) and /usr/lib/ (not aarch64-linux-gnu)
    cat > "$ENV_FILE" << EOF
# WendyOS CUDA Environment Configuration
# Auto-generated on $(date)
# Detected CUDA version: $cuda_ver

CUDA_VER=$cuda_ver
CUDA_HOME=/usr/local/cuda-$cuda_ver
PATH=/usr/local/cuda-$cuda_ver/bin:\$PATH
LD_LIBRARY_PATH=/usr/local/cuda-$cuda_ver/lib:/usr/lib
EOF

    chmod 644 "$ENV_FILE"
    log "CUDA environment file created at $ENV_FILE"
}

main "$@"
