#!/bin/bash
# setup-host-usb-link-local.sh — Configure the host for link-local on WendyOS USB gadget interfaces
#
# Detects whether the host uses NetworkManager or systemd-networkd and installs
# the appropriate configuration so USB NCM/ECM gadget interfaces automatically
# get a 169.254.x.x link-local address.
#
# Usage:
#   sudo ./setup-host-usb-link-local.sh [install|uninstall|status]

set -euo pipefail

SCRIPT_NAME="$(basename "$0")"

# Configuration file paths
NM_CONN="/etc/NetworkManager/system-connections/wendyos-usb.nmconnection"
NETWORKD_CONF="/etc/systemd/network/80-wendyos-usb.network"

# USB gadget drivers to match
USB_DRIVERS="cdc_ncm cdc_ether"

info()    { echo "[INFO]  $*"; }
warning() { echo "[WARN]  $*"; }
error()   { echo "[ERROR] $*" >&2; }

usage() {
    cat <<EOF
Usage: sudo $SCRIPT_NAME [install|uninstall|status]

Commands:
  install    Install link-local config for WendyOS USB interfaces (default)
  uninstall  Remove previously installed config
  status     Show current state

Detects NetworkManager or systemd-networkd automatically.
EOF
}

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        error "This script must be run as root (sudo)"
        exit 1
    fi
}

# Detect which network manager is active
detect_net_manager() {
    if systemctl is-active --quiet NetworkManager 2>/dev/null; then
        echo "networkmanager"
    elif systemctl is-active --quiet systemd-networkd 2>/dev/null; then
        echo "networkd"
    else
        echo "unknown"
    fi
}

# Check for existing NM connection profiles that could match USB gadget interfaces
check_existing_nm_profiles() {
    local conn_dir="/etc/NetworkManager/system-connections"
    local found=false

    if [ ! -d "$conn_dir" ]; then
        return
    fi

    # Look for profiles that match USB gadget interfaces by interface-name or driver
    for f in "$conn_dir"/*.nmconnection; do
        [ -f "$f" ] || continue
        # Skip our own profile
        [ "$f" = "$NM_CONN" ] && continue

        local name
        name=$(grep -m1 '^id=' "$f" 2>/dev/null | cut -d= -f2-)

        # Check if profile matches by USB gadget interface name
        if grep -qE 'interface-name=(usb[0-9]|enp.*u.*)' "$f" 2>/dev/null; then
            if [ "$found" = false ]; then
                warning "Found existing NM profiles that may match USB gadget interfaces:"
                found=true
            fi
            echo "  - $f ($name)"
        fi

        # Check if profile matches by CDC driver
        if grep -qE 'match\.driver=.*(cdc_ncm|cdc_ether)' "$f" 2>/dev/null; then
            if [ "$found" = false ]; then
                warning "Found existing NM profiles that may match USB gadget interfaces:"
                found=true
            fi
            echo "  - $f ($name)"
        fi
    done

    # Also check for generic "Wired connection" auto-created profiles (no interface match)
    # These match any wired interface including USB gadget
    for f in "$conn_dir"/*.nmconnection; do
        [ -f "$f" ] || continue
        [ "$f" = "$NM_CONN" ] && continue

        if grep -q '^type=ethernet' "$f" 2>/dev/null \
            && ! grep -qE '^interface-name=|^match\.' "$f" 2>/dev/null; then
            local name
            name=$(grep -m1 '^id=' "$f" 2>/dev/null | cut -d= -f2-)
            if [ "$found" = false ]; then
                warning "Found existing NM profiles that may match USB gadget interfaces:"
                found=true
            fi
            echo "  - $f ($name) — generic wired profile (no interface filter)"
        fi
    done

    if [ "$found" = true ]; then
        info "The wendyos-usb profile uses autoconnect-priority=100 and driver matching,"
        info "so it will take precedence over these profiles on USB gadget interfaces."
        info "Existing profiles are not modified."
        echo ""
    fi
}

install_networkmanager() {
    if [ -f "$NM_CONN" ]; then
        warning "Config already exists: $NM_CONN"
        info "Use '$SCRIPT_NAME uninstall' first to replace it"
        return 0
    fi

    # Check for existing NM profiles that might match USB gadget interfaces
    check_existing_nm_profiles

    info "Installing NetworkManager connection profile"
    cat > "$NM_CONN" <<'NMEOF'
[connection]
id=wendyos-usb
type=ethernet
autoconnect=true
autoconnect-priority=100
match.driver=cdc_ncm;cdc_ether

[ipv4]
method=link-local

[ipv6]
method=link-local
addr-gen-mode=stable-privacy
NMEOF
    chmod 0600 "$NM_CONN"

    info "Reloading NetworkManager connections"
    nmcli connection reload

    info "Installed: $NM_CONN"
    info "Any WendyOS USB device will now get a link-local address automatically"
}

install_networkd() {
    if [ -f "$NETWORKD_CONF" ]; then
        warning "Config already exists: $NETWORKD_CONF"
        info "Use '$SCRIPT_NAME uninstall' first to replace it"
        return 0
    fi

    info "Installing systemd-networkd config"
    cat > "$NETWORKD_CONF" <<'NDEOF'
[Match]
Driver=cdc_ncm cdc_ether

[Network]
LinkLocalAddressing=ipv4
IPv6LinkLocalAddressGenerationMode=stable-privacy
LLMNR=no
MulticastDNS=yes
NDEOF
    chmod 0644 "$NETWORKD_CONF"

    info "Reloading systemd-networkd"
    networkctl reload

    info "Installed: $NETWORKD_CONF"
    info "Any WendyOS USB device will now get a link-local address automatically"
}

uninstall_networkmanager() {
    if [ -f "$NM_CONN" ]; then
        rm -f "$NM_CONN"
        nmcli connection reload 2>/dev/null || true
        info "Removed: $NM_CONN"
    else
        info "Nothing to remove (NetworkManager config not found)"
    fi
}

uninstall_networkd() {
    if [ -f "$NETWORKD_CONF" ]; then
        rm -f "$NETWORKD_CONF"
        networkctl reload 2>/dev/null || true
        info "Removed: $NETWORKD_CONF"
    else
        info "Nothing to remove (systemd-networkd config not found)"
    fi
}

show_status() {
    local mgr
    mgr=$(detect_net_manager)

    echo "Network manager: $mgr"
    echo ""

    if [ -f "$NM_CONN" ]; then
        echo "NetworkManager config: $NM_CONN [INSTALLED]"
    else
        echo "NetworkManager config: $NM_CONN [not installed]"
    fi

    if [ -f "$NETWORKD_CONF" ]; then
        echo "systemd-networkd config: $NETWORKD_CONF [INSTALLED]"
    else
        echo "systemd-networkd config: $NETWORKD_CONF [not installed]"
    fi

    echo ""

    # Show any currently connected USB gadget interfaces
    local found=false
    for driver in $USB_DRIVERS; do
        for devpath in /sys/class/net/*/device/driver; do
            if [ -e "$devpath" ] && [ "$(basename "$(readlink "$devpath")")" = "$driver" ]; then
                local iface
                iface="$(basename "$(dirname "$(dirname "$devpath")")")"
                local addr
                addr=$(ip -4 addr show "$iface" 2>/dev/null | grep -oP 'inet \K[0-9.]+/[0-9]+' || echo "no address")
                echo "USB gadget interface: $iface ($driver) — $addr"
                found=true
            fi
        done
    done

    if [ "$found" = false ]; then
        echo "No USB gadget interfaces detected"
    fi

    # Show mDNS discovery if avahi-browse is available
    if command -v avahi-browse &>/dev/null; then
        echo ""
        echo "Scanning for WendyOS devices..."
        avahi-browse -t -p _wendyos._udp 2>/dev/null || echo "  No devices found"
    fi
}

do_install() {
    local mgr
    mgr=$(detect_net_manager)

    case "$mgr" in
        networkmanager)
            install_networkmanager
            ;;
        networkd)
            install_networkd
            ;;
        *)
            error "Could not detect NetworkManager or systemd-networkd"
            error "Please configure link-local manually (see docs/HOST-USB-LINK-LOCAL-SETUP.md)"
            exit 1
            ;;
    esac
}

do_uninstall() {
    # Remove both — doesn't hurt if one doesn't exist
    uninstall_networkmanager
    uninstall_networkd
}

main() {
    local cmd="${1:-install}"

    case "$cmd" in
        install)
            check_root
            do_install
            ;;
        uninstall)
            check_root
            do_uninstall
            ;;
        status)
            show_status
            ;;
        -h|--help|help)
            usage
            ;;
        *)
            error "Unknown command: $cmd"
            usage
            exit 1
            ;;
    esac
}

main "$@"
