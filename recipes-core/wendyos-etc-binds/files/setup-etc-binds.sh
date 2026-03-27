#!/bin/bash

# Setup bind mounts for persistent /etc files on /data partition
# This ensures device identity (UUID, hostname) persists across Mender OTA updates

set -e

DATA_ETC="/data/etc"
LOG_TAG="wendyos-etc-binds"
NM_CONNECTIONS="NetworkManager/system-connections"

log_info() {
    logger -t "${LOG_TAG}" -p user.info "$1"
    echo "[INFO] $1"
}

log_error() {
    logger -t "${LOG_TAG}" -p user.err "$1"
    echo "[ERROR] $1" >&2
}

# Verify /data is mounted
if ! mountpoint -q /data
then
    log_error "/data is not mounted, cannot setup bind mounts"
    exit 1
fi

log_info "Setting up persistent /etc bind mounts from /data"

# PHASE 1: identity files persistence (device-uuid, device-name)
log_info "Phase 1: Setting up identity files persistence"

# Create /data/etc/wendyos/ directory for identity files
if [ ! -d "${DATA_ETC}/wendyos" ]
then
    log_info "Creating ${DATA_ETC}/wendyos/ for persistent identity storage"
    mkdir -p "${DATA_ETC}/wendyos"
    chmod 755 "${DATA_ETC}/wendyos"
fi

# Bind-mount entire /etc/wendyos/ directory
# This persists device-uuid, device-name, and any other identity files
if ! mountpoint -q /etc/wendyos
then
    log_info "Bind-mounting ${DATA_ETC}/wendyos → /etc/wendyos"
    mount --bind "${DATA_ETC}/wendyos" /etc/wendyos

    if mountpoint -q /etc/wendyos
    then
        log_info "Successfully mounted /etc/wendyos"
    else
        log_error "Failed to bind-mount /etc/wendyos"
        exit 1
    fi
else
    log_info "/etc/wendyos already mounted, skipping"
fi

# [Note]
# /etc/hostname is NOT bind-mounted because file-level bind mounts prevent atomic
# writes. hostnamectl uses rename() for atomic updates, which fails with EBUSY on
# bind-mounted files. Instead, hostname is derived data that gets regenerated on
# every boot from the persistent device-name in /etc/wendyos/device-name.
# This approach matches industry patterns (Docker, CoreOS, Balena) where derived
# identities are computed from persisted seed data.
#

# PHASE 2:
log_info "Phase 2: Setting up NetworkManager user connections persistence"

# Create directory structure for NetworkManager user connections
# Note: distro-managed profiles (usb-gadget.nmconnection) live in
# /usr/lib/NetworkManager/system-connections/ (read-only, on rootfs).
# This directory is only for user-added connections (WiFi, VPN, etc.)
# which NM writes to /etc/NetworkManager/system-connections/.
if [ ! -d "${DATA_ETC}/${NM_CONNECTIONS}" ]
then
    log_info "Creating ${DATA_ETC}/${NM_CONNECTIONS}/ for persistent user connections"
    mkdir -p "${DATA_ETC}/${NM_CONNECTIONS}"
    chmod 755 "${DATA_ETC}/${NM_CONNECTIONS}"
fi

# Migration: sync distro-managed profiles between /usr/lib and /data.
#
# Distro profiles live in /usr/lib/NetworkManager/system-connections/ (rootfs,
# read-only, updated on every OTA). User-added/modified profiles live in
# /data/etc/NetworkManager/system-connections/ (persistent, bind-mounted to /etc).
# NM gives /etc priority over /usr/lib for same-named files.
#
# Problem: if /data has a stale distro copy, it shadows the /usr/lib version
# and blocks OTA updates from taking effect. But if the user intentionally
# modified the profile via nmcli, we must preserve their changes.
#
# Solution: track the distro version hash (like dpkg conffiles). On each boot:
#   - If /data file hash matches the stored distro hash → not user-modified → remove
#   - If /data file hash differs from stored hash → user modified → keep
#   - Update the stored hash from the current /usr/lib version
#
NM_LIB_CONNECTIONS="/usr/lib/NetworkManager/system-connections"
NM_HASH_DIR="${DATA_ETC}/${NM_CONNECTIONS}/.distro-hashes"

# Migration runs with set +e so failures (disk full, I/O error) never abort the
# script before the bind mounts below are established. Migration is best-effort;
# bind mounts are critical for device operation.
if [ -d "${NM_LIB_CONNECTIONS}" ] && [ -d "${DATA_ETC}/${NM_CONNECTIONS}" ]
then
    set +e
    mkdir -p "${NM_HASH_DIR}" 2>/dev/null

    for lib_file in "${NM_LIB_CONNECTIONS}"/*.nmconnection
    do
        [ -f "${lib_file}" ] || continue
        local_name=$(basename "${lib_file}")
        data_file="${DATA_ETC}/${NM_CONNECTIONS}/${local_name}"
        hash_file="${NM_HASH_DIR}/${local_name}.sha256"

        if [ -f "${data_file}" ]
        then
            stored_hash=""
            if [ -f "${hash_file}" ]
            then
                stored_hash=$(cat "${hash_file}" 2>/dev/null) || stored_hash=""
            fi

            if [ -n "${stored_hash}" ]
            then
                # Hash file exists and is valid: compare /data file against
                # the stored distro hash. If they match, the user never
                # modified it → safe to remove.
                data_hash=$(sha256sum "${data_file}" 2>/dev/null | awk '{print $1}') || data_hash=""
                if [ -n "${data_hash}" ] && [ "${data_hash}" = "${stored_hash}" ]
                then
                    log_info "Removing stale distro profile from /data: ${local_name}"
                    rm -f "${data_file}"
                    rm -f "${data_file}.backup" "${data_file}~" "${data_file}.un~" \
                          "${DATA_ETC}/${NM_CONNECTIONS}/.${local_name}.un~"
                else
                    log_info "Keeping user-modified profile in /data: ${local_name}"
                fi
            else
                # No valid hash: first migration from the old system, or
                # hash file was corrupted (e.g., power loss during write).
                # The /data copy was originally installed by setup-etc-binds
                # (not user-created), so it is safe to remove.
                log_info "First migration — removing old distro profile from /data: ${local_name}"
                rm -f "${data_file}"
                rm -f "${data_file}.backup" "${data_file}~" "${data_file}.un~" \
                      "${DATA_ETC}/${NM_CONNECTIONS}/.${local_name}.un~"
            fi
        fi

        # Update the stored hash from the current /usr/lib version so the next
        # boot can detect whether the user has modified the profile.
        # Atomic write: write to temp file then rename to avoid corruption on
        # power failure.
        if sha256sum "${lib_file}" 2>/dev/null | awk '{print $1}' > "${hash_file}.tmp" 2>/dev/null
        then
            mv "${hash_file}.tmp" "${hash_file}" 2>/dev/null || \
                log_error "Failed to update hash for ${local_name}"
        else
            log_error "Failed to compute hash for ${local_name} (disk full?)"
            rm -f "${hash_file}.tmp" 2>/dev/null
        fi
    done

    set -e
fi

# Bind-mount NetworkManager system-connections directory
# This persists user-added connections (WiFi, VPN) across OTA updates.
# Distro profiles in /usr/lib/NetworkManager/system-connections/ are not
# affected — they come from the rootfs and are updated on every OTA.
if ! mountpoint -q "/etc/${NM_CONNECTIONS}"
then
    log_info "Bind-mounting ${DATA_ETC}/${NM_CONNECTIONS} → /etc/${NM_CONNECTIONS}"
    mount --bind "${DATA_ETC}/${NM_CONNECTIONS}" "/etc/${NM_CONNECTIONS}"

    if mountpoint -q "/etc/${NM_CONNECTIONS}"
    then
        log_info "Successfully mounted /etc/${NM_CONNECTIONS}"
    else
        log_error "Failed to bind-mount /etc/${NM_CONNECTIONS}"
        exit 1
    fi
else
    log_info "/etc/${NM_CONNECTIONS} already mounted, skipping"
fi

# Verification...
log_info "Verifying all bind mounts are active"

MOUNT_CHECKS=(
    "/etc/wendyos"
    "/etc/${NM_CONNECTIONS}"
)

FAILED=0
for mount_point in "${MOUNT_CHECKS[@]}"
do
    if mountpoint -q "${mount_point}"
    then
        log_info "✓ ${mount_point} is mounted"
    else
        log_error "✗ ${mount_point} is NOT mounted"
        FAILED=1
    fi
done

if [ ${FAILED} -eq 0 ]
then
    log_info "All bind mounts successfully configured"
    exit 0
else
    log_error "Some bind mounts failed, check logs"
    exit 1
fi
