#!/usr/bin/env bash
#
# Manage internet sharing to devices connected via USB gadget
# (the host machine runs a DHCP server)
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m' # No Color

# Global flags
VERBOSE=false
DRY_RUN=false

# Print formatted/colored output
info() { printf "%b\n" "$*"; }
success() { printf "%b\n" "$*"; }
warning() { printf "%bWARNING%b: %s\n" "${YELLOW}" "${NC}" "$*"; }
error() { printf "%bERROR%b: %s\n" "${RED}" "${NC}" "$*"; }
debug() {
    if [[ "${VERBOSE}" == "true" ]]; then
        printf "%bDEBUG%b: %s\n" "${BLUE}" "${NC}" "$*" >&2
    fi
}

# Execute command or show what would be executed in dry-run mode
# Usage: execute "command with args and redirections"
execute() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        printf "%bDRY-RUN%b: %s\n" "${YELLOW}" "${NC}" "$*"
        return 0
    else
        eval "$*"
    fi
}

# Check if a command is available
check_command() {
    command -v "$1" &> /dev/null
}

# Check for all required tools
check_required_tools() {
    local missing_tools=()
    local required_tools=(
        "nmcli"      # NetworkManager CLI
        "ip"         # Network interface management
        "grep"       # Pattern matching
        "awk"        # Text processing
        "cat"        # Read files
        "readlink"   # Read symbolic links
        "basename"   # Extract filename from path
        "cut"        # Field extraction
        "head"       # First lines of output
        "tail"       # Last lines of output
        "ping"       # Network connectivity test
        "sudo"       # Elevated privileges
    )

    for tool in "${required_tools[@]}"
    do
        if ! check_command "${tool}"
        then
            missing_tools+=("${tool}")
        fi
    done

    if [[ ${#missing_tools[@]} -gt 0 ]]
    then
        error "Missing required tools: ${missing_tools[*]}"
        echo ""
        echo "Please install the missing tools and try again."
        echo "On Debian/Ubuntu: sudo apt install ${missing_tools[*]}"
        echo "On Fedora/RHEL:   sudo dnf install ${missing_tools[*]}"
        echo "On Arch:          sudo pacman -S ${missing_tools[*]}"
        exit 1
    fi
}

# Check all required tools at startup
check_required_tools

# Check if we can use sudo (will be needed for nmcli commands)
check_sudo() {
    if ! sudo -n true 2>/dev/null
    then
        warning "Some commands require sudo privileges. You may be prompted for your password."
    fi
}

# check if a USB network interface is a gadget device
is_gadget_device() {
    local dev_path="$1"

    # [Note] '${dev_path}/device' is a simlink!
    local usb_device_path="${dev_path}/device/.."
    local manufacturer=""
    local product=""
    local idvendor=""
    local idproduct=""

    # Read USB device attributes from parent device (not interface)
    # The device/ symlink points to USB interface, we need the parent USB device
    if [[ -f "${usb_device_path}/manufacturer" ]]
    then
        manufacturer=$(cat "${usb_device_path}/manufacturer" 2>/dev/null || echo "")
    fi

    if [[ -f "${usb_device_path}/product" ]]
    then
        product=$(cat "${usb_device_path}/product" 2>/dev/null || echo "")
    fi

    if [[ -f "${usb_device_path}/idVendor" ]]
    then
        idvendor=$(cat "${usb_device_path}/idVendor" 2>/dev/null || echo "")
    fi

    if [[ -f "${usb_device_path}/idProduct" ]]
    then
        idproduct=$(cat "${usb_device_path}/idProduct" 2>/dev/null || echo "")
    fi

    # Debug: print USB device attributes
    debug "    Manufacturer: ${manufacturer:-<not found>}"
    debug "    Product:      ${product:-<not found>}"
    debug "    USB ID:       ${idvendor:-????}:${idproduct:-????}"

    # Check if it matches our gadget device characteristics
    # Approach 1: Check manufacturer (most reliable)
    if [[ "${manufacturer}" == *"Wendy"* ]] || [[ "${manufacturer}" == *"wendy"* ]]
    then
        return 0
    fi

    # Approach 2: Check product name
    if [[ "${product}" == *"WendyOS"* ]] || [[ "${product}" == *"Jetson"* ]]
    then
        return 0
    fi

    # Approach 3: Check USB IDs (Linux Foundation Multifunction Composite Gadget)
    if [[ "${idvendor}" == "1d6b" ]] && [[ "${idproduct}" == "0104" ]]
    then
        return 0
    fi

    return 1
}

# list all USB gadget devices (with or without carrier)
list_gadget_devices() {
    local dev_name=""
    local subsys=""
    local carrier=""
    local found_any=false

    debug "Scanning for USB gadget devices..."
    for dev in /sys/class/net/*
    do
        [[ -e "${dev}/device/subsystem" ]] || continue
        subsys=$(readlink "${dev}/device/subsystem" 2>/dev/null || echo "")

        debug "Checking ${dev}..."

        # Check if it's a USB device
        if [[ "${subsys}" == *"usb"* ]]
        then
            debug "  Found USB device: ${dev}"

            # Check if it's specifically a gadget device
            if is_gadget_device "${dev}"
            then
                dev_name=$(basename "${dev}")
                carrier=$(cat "${dev}/carrier" 2>/dev/null || echo "0")
                found_any=true

                if [[ "${carrier}" == "1" ]]; then
                    info "  ${dev_name} ${GREEN}[LINK UP]${NC}"
                else
                    info "  ${dev_name} ${YELLOW}[LINK DOWN]${NC}"
                fi
            else
                debug "  Not a gadget device"
            fi
        fi
    done

    if [[ "${found_any}" == "false" ]]; then
        echo "  No USB gadget devices found"
        return 1
    fi
}

# detect the USB gadget interface (returns first one with active carrier)
detect_gadget_interface() {
    local iface=""
    local dev_name=""
    local subsys=""
    local found_usb=false
    local found_gadget_no_carrier=false

    # debug "Detecting USB gadget interface..."

    # Look for USB gadget device with active carrier in sysfs
    for dev in /sys/class/net/*
    do
        [[ -e "${dev}/device/subsystem" ]] || continue
        subsys=$(readlink "${dev}/device/subsystem" 2>/dev/null || echo "")

        # Check if it's a USB device
        if [[ "${subsys}" == *"usb"* ]]
        then
            found_usb=true
            dev_name=$(basename "${dev}")
            debug "Found USB device: ${dev_name}"

            # Check if it's specifically a gadget device
            if is_gadget_device "${dev}"
            then
                debug "  Gadget device"

                # Only return device with active carrier (link up and working)
                if [[ -e "${dev}/carrier" ]] && [[ $(cat "${dev}/carrier" 2>/dev/null || echo "0") == "1" ]]
                then
                    debug "  Device has active carrier"
                    iface="${dev_name}"
                    break
                else
                    debug "  Device has no carrier"
                    found_gadget_no_carrier=true
                fi
            else
                debug "  Device is not a gadget"
            fi
        fi
    done

    # Provide specific error feedback
    if [[ -z "${iface}" ]]; then
        if [[ "${found_gadget_no_carrier}" == "true" ]]; then
            debug "ERROR: Gadget device found but link is down"
        elif [[ "${found_usb}" == "true" ]]; then
            debug "ERROR: USB devices found but none are gadget devices"
        else
            debug "ERROR: No USB devices found"
        fi
    fi

    echo "${iface}"
}

# detect active internet interface
detect_internet_interface() {
    # get the interface with the default route
    ip route show default | awk '/default/ {print $5; exit}'
}

# check if internet sharing is already enabled on the specified interface
check_sharing_status() {
    local iface="$1"    # gadget interface to be checked
    local conn_name
    local method

    conn_name=$(nmcli -t -f DEVICE,CONNECTION device status | grep "^${iface}:" | cut -d: -f2)
    if [[ -z "${conn_name}" ]]
    then
        # No NetworkManager connection found for the specified interface
        echo ""
        return
    fi

    method=$(nmcli -t -f ipv4.method connection show "${conn_name}" 2>/dev/null | cut -d: -f2)
    if [[ "${method}" == "shared" ]]
    then
        # internet sharing is enabled
        echo "enabled:${conn_name}"
    else
        # internet sharing is disabled
        echo "disabled:${conn_name}"
    fi
}

# enable internet sharing
enable_sharing() {
    local gadget_iface="$1"
    local internet_iface="$2"
    local status=""
    local conn_name=""

    info "Enabling internet sharing on '${BOLD}${gadget_iface}${NC}'..."

    # Check current sharing status
    status=$(check_sharing_status "${gadget_iface}")
    if [[ -z "${status}" ]]
    then
        # Create a new connection
        conn_name="usb-gadget-sharing"
        info "Create '${conn_name}' connection..."
        # Remove any stale connection with the same name to avoid duplicates
        execute "sudo nmcli connection delete '${conn_name}' 2>/dev/null || true"
        local cmd="sudo nmcli connection add \
            type ethernet \
            ifname '${gadget_iface}' \
            con-name '${conn_name}' \
            ipv4.method shared \
            connection.autoconnect yes \
            connection.autoconnect-priority 100"
        execute "${cmd}"
    else
        # Existing connection found
        local status_type=""

        status_type=$(echo "${status}" | cut -d: -f1)
        conn_name=$(echo "${status}" | cut -d: -f2-)

        if [[ "${status_type}" == "enabled" ]]
        then
            info "Connection: '${conn_name}'"
            success "Internet sharing already enabled on '${gadget_iface}'"
            return 0
        fi

        # Modify existing connection
        info "Update connection: '${conn_name}'"
        execute "sudo nmcli connection modify '${conn_name}' ipv4.method shared"
        execute "sudo nmcli connection modify '${conn_name}' connection.autoconnect yes"
    fi

    # Bring up the connection
    info "Activate connection '${conn_name}' ..."
    execute "sudo nmcli connection down '${conn_name}' 2>/dev/null || true"
    execute "sleep 1"
    execute "sudo nmcli connection up '${conn_name}'"

    # Wait a moment for configuration to apply
    execute "sleep 2"

    # Skip verification in dry-run mode
    if [[ "${DRY_RUN}" == "true" ]]; then
        success "Would enable internet sharing on '${gadget_iface}'"
        return 0
    fi

    # Verify configuration
    local host_ip=""

    host_ip=$(ip addr show "${gadget_iface}" | grep "inet " | awk '{print $2}' | cut -d/ -f1)
    if [[ -n "${host_ip}" ]]
    then
        success "Internet sharing successfully enabled!"
        echo ""
        echo "Configuration:"
        echo "  Connection:       ${conn_name}"
        echo "  Gadget interface: ${gadget_iface}"
        echo "  Host IP:          ${host_ip}"
        return 0
    else
        error "Failed to configure IP address on '${gadget_iface}'"
        return 1
    fi
}

# disable internet sharing
disable_sharing() {
    local gadget_iface="$1"

    info "Disabling internet sharing on ${gadget_iface}..."

    # Find connections with method=shared on this interface
    local status=""

    status=$(check_sharing_status "${gadget_iface}")
    if [[ -z "${status}" ]]
    then
        info "No connection found on ${gadget_iface}"
        return 0
    fi

    local status_type=""
    local conn_name=""

    status_type=$(echo "${status}" | cut -d: -f1)
    conn_name=$(echo "${status}" | cut -d: -f2-)
    if [[ "${status_type}" != "enabled" ]]
    then
        info "Internet sharing is not enabled on ${gadget_iface}"
        return 0
    fi

    # Disable sharing
    info "Update connection '${conn_name}'..."
    execute "sudo nmcli connection modify '${conn_name}' ipv4.method auto"
    execute "sudo nmcli connection down '${conn_name}' 2>/dev/null || true"
    execute "sudo nmcli connection up '${conn_name}' 2>/dev/null || true"

    if [[ "${DRY_RUN}" == "true" ]]; then
        success "Would disable internet sharing on ${gadget_iface}"
    else
        success "Internet sharing disabled on ${gadget_iface}"
    fi
    return 0
}

# show status
show_status() {
    local gadget_iface="$1"
    local status
    local status_type
    local conn_name

    info "Internet sharing status:"
    # echo ""

    status=$(check_sharing_status "${gadget_iface}")
    if [[ -z "${status}" ]]
    then
        warning "No NetworkManager connection found!"
    else
        status_type=$(echo "${status}" | cut -d: -f1)
        conn_name=$(echo "${status}" | cut -d: -f2-)

        if [[ "${status_type}" == "enabled" ]]
        then
            info "  Connection:       '${BOLD}${conn_name}${NC}'"
            info "  Internet sharing: ${GREEN}${BOLD}ENABLED${NC}"

            local host_ip
            local client_ip

            host_ip=$(ip addr show "${gadget_iface}" | grep "inet " | awk '{print $2}' | cut -d/ -f1)
            if [[ -n "${host_ip}" ]]
            then
                info "  Host IP:          ${BOLD}${host_ip}${NC}"
            fi

            # Check if the board is connected
            client_ip=$(ip neigh show dev "${gadget_iface}" | grep "10.42.0" | grep -E "REACHABLE|STALE" | awk '{print $1}' | head -1)
            if [[ -n "${client_ip}" ]]
            then
                info "  Board IP:         ${BOLD}${client_ip}${NC}"
            else
                info "Waiting for the board to connect..."
            fi
        else
            info "  Connection:       '${BOLD}${conn_name}${NC}'"
            info "  Internet sharing: ${YELLOW}${BOLD}DISABLED${NC}"
            echo "  Method: $(nmcli -t -f ipv4.method connection show "${conn_name}" | cut -d: -f2)"
        fi
    fi

    echo ""
}

# test internet connectivity through gadget interface
test_connectivity() {
    local gadget_iface="$1"

    info "Testing internet connectivity through ${gadget_iface}..."

    # Check if interface is up
    if ! ip link show "${gadget_iface}" | grep -q "UP"
    then
        error "Interface ${gadget_iface} is not UP"
        return 1
    fi

    # Check if we have an IP
    local host_ip
    host_ip=$(ip addr show "${gadget_iface}" | grep "inet " | awk '{print $2}' | cut -d/ -f1)
    if [[ -z "${host_ip}" ]]
    then
        error "No IP address on ${gadget_iface}"
        return 1
    fi

    info "Host IP: ${host_ip}"

    # Check if client is connected
    local client_ip
    client_ip=$(ip neigh show dev "${gadget_iface}" | grep -E "REACHABLE|STALE" | awk '{print $1}' | head -1)
    if [[ -z "${client_ip}" ]]
    then
        warning "No client connected to ${gadget_iface}"
    else
        info "Client IP: ${client_ip}"

        # Try to ping client
        info "Pinging client..."
        if ping -c 2 -W 2 -I "${gadget_iface}" "${client_ip}" &>/dev/null
        then
            success "Client is reachable"
        else
            warning "Cannot ping client"
        fi
    fi

    # Test internet connectivity from host
    info "Test host internet connectivity..."
    if ping -c 2 -W 2 8.8.8.8 &>/dev/null
    then
        success "Host has internet connectivity"
    else
        error "Host has no internet connectivity"
        return 1
    fi

    echo ""
    success "Connectivity test completed"
}

# show usage
show_usage() {
    cat << EOF
Usage: $0 [options] <command> [interface]

Commands:
    enable   Enable internet sharing to gadget device
    disable  Disable internet sharing to gadget device
    status   Show current internet sharing status
    list     List all detected USB gadget devices
    test     Test internet connectivity through gadget interface

Options:
    -v, --verbose   Enable verbose/debug output
    -n, --dry-run   Show what would be done without doing it
    -h, --help      Show this help message

Arguments:
    interface  Specify USB interface (auto-detected if not provided)

Examples:
    $0 list                      # List all gadget devices
    $0 enable                    # Auto-detect and enable
    $0 --verbose enable          # Enable with debug output
    $0 --dry-run enable          # Preview what would be done
    $0 status                    # Show status
    $0 test                      # Test connectivity
    $0 enable enxc23c5a0a423e    # Enable on specific interface

EOF
}

# Main script logic
main() {
    local command=""
    local gadget_iface=""

    # Parse flags and arguments
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -v|--verbose)
                VERBOSE=true
                shift
                ;;
            -n|--dry-run)
                DRY_RUN=true
                shift
                ;;
            -h|--help)
                show_usage
                exit 0
                ;;
            enable|disable|status|list|test)
                command="$1"
                shift
                ;;
            *)
                # Assume it's the interface name
                gadget_iface="$1"
                shift
                ;;
        esac
    done

    if [[ -z "${command}" ]]; then
        show_usage
        exit 1
    fi

    debug "Verbose mode enabled"
    if [[ "${DRY_RUN}" == "true" ]]; then
        info "dry run mode enabled - no changes will be made"
    fi

    # Handle list command (doesn't need interface detection)
    if [[ "${command}" == "list" ]]; then
        info "USB gadget devices:"
        list_gadget_devices
        exit $?
    fi

    # Check sudo availability for other commands
    check_sudo

    # Detect or use provided interface
    if [[ -z "${gadget_iface}" ]]; then
        info "Detecting USB gadget interface..."
        gadget_iface=$(detect_gadget_interface)
    fi

    if [[ -z "${gadget_iface}" ]]; then
        error "No USB gadget interface detected!"
        echo ""
        echo "Please ensure:"
        echo "  1. The board/target is connected via USB"
        echo "  2. USB gadget is properly configured on the target"
        echo "  3. USB interface is UP with active carrier"
        echo ""
        echo "Or specify the interface manually:"
        echo "  $0 ${command} <interface>"
        echo ""
        echo "Available network interfaces:"
        ip -br link show
        echo ""
        info "Tip: Use '$0 list' to see all gadget devices"
        exit 1
    fi

    info "USB gadget interface: '${BOLD}${gadget_iface}${NC}'"
    echo ""

    case "${command}" in
        enable)
            local internet_iface=""

            internet_iface=$(detect_internet_interface)
            if [[ -z "${internet_iface}" ]]
            then
                warning "Could not detect active internet connection."
                echo "Continuing anyway, but internet sharing may not work..."
            else
                info "Internet interface: ${internet_iface}"
                # Verify internet connectivity
                if ! ping -c 1 -W 2 8.8.8.8 &>/dev/null
                then
                    warning "Cannot reach internet on ${internet_iface}"
                    echo "Continuing anyway..."
                else
                    success "Internet connection verified"
                fi
            fi
            echo ""
            enable_sharing "${gadget_iface}" "${internet_iface}"
            ;;
        disable)
            disable_sharing "${gadget_iface}"
            ;;
        status)
            show_status "${gadget_iface}"
            ;;
        test)
            test_connectivity "${gadget_iface}"
            ;;
        *)
            error "Unknown command: ${command}"
            echo ""
            show_usage
            exit 1
            ;;
    esac
}

main "$@"
