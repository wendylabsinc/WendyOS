#!/bin/bash

set -Eeuo pipefail

PAYLOAD_NAME="wendy-install"
DATA_CONFIG_ROOT="/data/wendyos-install-config"
ROOT_CONFIG_ROOT="/var/lib/wendyos-install-config"
SSH_DROPIN_DIR="/etc/ssh/sshd_config.d"
SSH_DROPIN="${SSH_DROPIN_DIR}/90-wendy-install.conf"
NETWORK_DIR="/etc/NetworkManager/system-connections"
WENDYOS_DIR="/etc/wendyos"

log() {
    logger -t wendyos-install-config "$*" || true
    echo "[wendyos-install-config] $*"
}

persistent_config_root() {
    if mountpoint -q /data; then
        echo "${DATA_CONFIG_ROOT}"
        return 0
    fi

    echo "${ROOT_CONFIG_ROOT}"
}

persistent_payload_dir() {
    echo "$(persistent_config_root)/payload"
}

ensure_persistent_config_root() {
    local root

    root="$(persistent_config_root)"
    mkdir -p "${root}"
    chmod 0700 "${root}"
}

find_payload_dir() {
    local root candidate
    for root in /boot /boot/efi /boot/EFI
    do
        if [ ! -d "${root}" ]; then
            continue
        fi

        candidate="$(find "${root}" -maxdepth 2 -type f -path "*/${PAYLOAD_NAME}/config.json" 2>/dev/null | head -n 1 || true)"
        if [ -n "${candidate}" ]; then
            dirname "${candidate}"
            return 0
        fi
    done
    return 1
}

persist_boot_payload() {
    local payload_dir="$1"
    local persistent_dir="$2"

    ensure_persistent_config_root

    rm -rf "${persistent_dir}"
    mkdir -p "${persistent_dir}"
    cp -a "${payload_dir}/." "${persistent_dir}/"
    find "${persistent_dir}" -type d -exec chmod 0700 {} +
    find "${persistent_dir}" -type f -exec chmod 0600 {} +

    log "Persisted installer payload to ${persistent_dir}" >&2
}

load_payload_dir() {
    local boot_payload_dir persistent_dir

    if boot_payload_dir="$(find_payload_dir)"; then
        persistent_dir="$(persistent_payload_dir)"
        persist_boot_payload "${boot_payload_dir}" "${persistent_dir}"
        rm -rf "${boot_payload_dir}" || true
        echo "${persistent_dir}"
        return 0
    fi

    persistent_dir="$(persistent_payload_dir)"
    if [ -f "${persistent_dir}/config.json" ]; then
        echo "${persistent_dir}"
        return 0
    fi

    return 1
}

json_value() {
    local payload_dir="$1"
    local query="$2"
    jq -r "${query} // empty" "${payload_dir}/config.json"
}

ensure_user() {
    local user="$1"
    if id -u "${user}" >/dev/null 2>&1; then
        return 0
    fi

    log "Creating SSH user ${user}"
    useradd -m -d "/home/${user}" -s /bin/bash -G dialout,video,audio,users "${user}"
}

ensure_user_home() {
    local user="$1"
    local home="/home/${user}"
    local uid gid

    uid="$(id -u "${user}")"
    gid="$(id -g "${user}")"

    mkdir -p "${home}/.ssh"
    chmod 0755 "${home}"
    chmod 0700 "${home}/.ssh"

    if [ ! -f "${home}/.bashrc" ]; then
        cat > "${home}/.bashrc" << 'EOF'
# WendyOS User Environment
if [ -f /etc/profile ]; then
    . /etc/profile
fi
EOF
        chmod 0644 "${home}/.bashrc"
    fi

    if [ ! -f "${home}/.profile" ]; then
        cat > "${home}/.profile" << 'EOF'
if [ -f "$HOME/.bashrc" ]; then
    . "$HOME/.bashrc"
fi
EOF
        chmod 0644 "${home}/.profile"
    fi

    chown -R "${uid}:${gid}" "${home}"
}

apply_hostname() {
    local payload_dir="$1"
    local hostname suffix

    hostname="$(json_value "${payload_dir}" '.hostname')"
    if [ -z "${hostname}" ]; then
        return 0
    fi

    if [[ ! "${hostname}" =~ ^wendyos-[a-z0-9]([a-z0-9-]{0,53}[a-z0-9])?$ ]]; then
        log "Skipping invalid hostname ${hostname}"
        return 0
    fi

    suffix="${hostname#wendyos-}"
    mkdir -p "${WENDYOS_DIR}"
    echo "${suffix}" > "${WENDYOS_DIR}/device-name"
    chmod 0644 "${WENDYOS_DIR}/device-name"
    log "Seeded device name ${suffix}"
}

apply_wifi() {
    local payload_dir="$1"
    local profile

    if [ ! -d "${payload_dir}/network" ]; then
        return 0
    fi

    mkdir -p "${NETWORK_DIR}"
    find "${payload_dir}/network" -type f -name '*.nmconnection' | while read -r profile
    do
        install -m 0600 "${profile}" "${NETWORK_DIR}/$(basename "${profile}")"
        log "Installed Wi-Fi profile $(basename "${profile}")"
    done
}

apply_agent_binary() {
    local payload_dir="$1"
    local agent_binary="${payload_dir}/assets/wendy-agent"

    if [ ! -f "${agent_binary}" ]; then
        return 0
    fi

    install -m 0755 "${agent_binary}" /usr/local/bin/wendy-agent
    systemctl disable --now wendyos-agent-updater.timer wendyos-agent-updater.service 2>/dev/null || true
    log "Installed injected wendy-agent binary"
}

apply_certs() {
    local payload_dir="$1"

    if [ ! -d "${payload_dir}/certs" ]; then
        return 0
    fi

    mkdir -p /etc/wendy-agent
    cp -a "${payload_dir}/certs/." /etc/wendy-agent/
    find /etc/wendy-agent -type d -exec chmod 0700 {} +
    find /etc/wendy-agent -type f -exec chmod 0600 {} +
    log "Installed provisioned wendy-agent certificates"
}

ssh_service_action() {
    local action="$1"
    systemctl "${action}" ssh.service 2>/dev/null || true
    systemctl "${action}" sshd.service 2>/dev/null || true
}

write_ssh_dropin() {
    local password_auth="$1"
    local pubkey_auth="$2"

    mkdir -p "${SSH_DROPIN_DIR}"
    cat > "${SSH_DROPIN}" << EOF
PasswordAuthentication ${password_auth}
PubkeyAuthentication ${pubkey_auth}
PermitRootLogin no
EOF
}

apply_ssh() {
    local payload_dir="$1"
    local mode user password auth_keys uid gid home

    mode="$(json_value "${payload_dir}" '.ssh.mode')"
    user="$(json_value "${payload_dir}" '.ssh.username')"
    password="$(json_value "${payload_dir}" '.ssh.password')"

    case "${mode}" in
        ""|"default")
            return 0
            ;;
        "disable")
            rm -f "${SSH_DROPIN}"
            ssh_service_action disable
            log "Disabled SSH service"
            return 0
            ;;
        "password")
            [ -n "${user}" ] || user="wendy"
            if [ -z "${password}" ]; then
                log "Skipping SSH password setup because no password was provided"
                return 0
            fi
            ensure_user "${user}"
            ensure_user_home "${user}"
            echo "${user}:${password}" | chpasswd
            write_ssh_dropin yes yes
            ssh_service_action enable
            log "Enabled SSH password auth for ${user}"
            return 0
            ;;
        "key")
            [ -n "${user}" ] || user="wendy"
            ensure_user "${user}"
            ensure_user_home "${user}"
            auth_keys="${payload_dir}/ssh/authorized_keys"
            if [ ! -f "${auth_keys}" ]; then
                log "Skipping SSH key setup because authorized_keys payload is missing"
                return 0
            fi
            home="/home/${user}"
            uid="$(id -u "${user}")"
            gid="$(id -g "${user}")"
            install -m 0600 "${auth_keys}" "${home}/.ssh/authorized_keys"
            chown "${uid}:${gid}" "${home}/.ssh/authorized_keys"
            write_ssh_dropin no yes
            ssh_service_action enable
            log "Enabled SSH public key auth for ${user}"
            return 0
            ;;
        *)
            log "Skipping unknown SSH mode ${mode}"
            return 0
            ;;
    esac
}

main() {
    local payload_dir
    if ! payload_dir="$(load_payload_dir)"; then
        log "No installer payload present"
        exit 0
    fi

    log "Applying installer payload from ${payload_dir}"

    apply_hostname "${payload_dir}"
    apply_wifi "${payload_dir}"
    apply_agent_binary "${payload_dir}"
    apply_certs "${payload_dir}"
    apply_ssh "${payload_dir}"

    log "Installer payload applied successfully"
}

main "$@"
