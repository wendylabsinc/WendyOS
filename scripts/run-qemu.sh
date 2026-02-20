#!/usr/bin/env bash
#
# Launch WendyOS in QEMU with networking
# This script sets up the network if needed and launches QEMU
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

# Configuration
BUILD_DIR="${BUILD_DIR:-build}"
MACHINE="qemuarm64-wendyos"
BRIDGE="br-wendyos"
TAP="tap-wendyos"
USB_DEVICES=()  # Array of USB devices to pass through (vendor:product format)

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

# Execute a command, with dry-run support
execute() {
    if [[ "${DRY_RUN}" == "true" ]]
    then
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
        "qemu-system-aarch64"  # QEMU ARM64 emulator
        "ip"                   # Network interface management
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
        echo "On Debian/Ubuntu: sudo apt install qemu-system-arm"
        echo "On Fedora/RHEL:   sudo dnf install qemu-system-aarch64"
        echo "On Arch:          sudo pacman -S qemu-system-aarch64"
        exit 1
    fi
}

# Detect script and workspace directories
detect_directories() {
    local script_dir
    local meta_dir
    local workspace_dir

    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    meta_dir="$(dirname "$script_dir")"
    workspace_dir="$(dirname "$meta_dir")"

    debug "Script directory: ${script_dir}"
    debug "Meta directory: ${meta_dir}"
    debug "Workspace directory: ${workspace_dir}"

    echo "${workspace_dir}"
}

# Check if image exists
check_image_exists() {
    local deploy_dir="$1"
    local image="$2"
    local kernel="$3"

    if [[ ! -f "${deploy_dir}/${image}" ]]
    then
        error "Image not found: ${deploy_dir}/${image}"
        echo ""
        echo "Please build the image first:"
        echo "  cd ${BUILD_DIR}"
        echo "  source ../repos/poky/oe-init-build-env"
        echo "  bitbake wendyos-image"
        return 1
    fi

    if [[ ! -f "${deploy_dir}/${kernel}" ]]
    then
        error "Kernel not found: ${deploy_dir}/${kernel}"
        return 1
    fi

    return 0
}

# Check if network is ready
check_network_ready() {
    if ! ip link show "${BRIDGE}" &>/dev/null
    then
        return 1
    fi

    if ! ip link show "${TAP}" &>/dev/null
    then
        return 1
    fi

    if ! ip link show "${BRIDGE}" | grep -q "UP"
    then
        return 1
    fi

    return 0
}

# Setup network if needed
setup_network_if_needed() {
    local script_dir="$1"

    if check_network_ready
    then
        debug "Network already configured"
        return 0
    fi

    info "Setting up QEMU network..."
    echo ""

    local setup_script="${script_dir}/manage-qemu-network-host.sh"
    if [[ ! -f "${setup_script}" ]]
    then
        error "Network setup script not found: ${setup_script}"
        return 1
    fi

    local setup_args="setup"
    if [[ "${DRY_RUN}" == "true" ]]
    then
        setup_args="--dry-run ${setup_args}"
    fi

    if ! ${setup_script} ${setup_args}
    then
        error "Failed to setup network"
        return 1
    fi

    echo ""
}

# Launch QEMU
launch_qemu() {
    local image_path="$1"
    local kernel_path="$2"

    info "Launching WendyOS in QEMU..."
    echo ""
    info "  ${BOLD}Image:${NC}      $(basename "${image_path}")"
    info "  ${BOLD}Kernel:${NC}     $(basename "${kernel_path}")"
    info "  ${BOLD}Machine:${NC}    virt (ARM64 Cortex-A57)"
    info "  ${BOLD}Memory:${NC}     4096 MB"
    info "  ${BOLD}CPUs:${NC}       4"
    info "  ${BOLD}Network:${NC}    ${TAP} -> ${BRIDGE} (10.42.0.1)"
    echo ""
    info "Guest will receive IP via DHCP (10.42.0.x)"
    echo ""

    if [[ "${DRY_RUN}" != "true" ]]
    then
        success "To exit QEMU: ${BOLD}Ctrl-A, then X${NC}"
        echo ""
        echo "---"
        echo ""
    fi

    local cmd
    cmd="qemu-system-aarch64"
    cmd+=" -machine virt"
    cmd+=" -cpu cortex-a57"
    cmd+=" -smp 4"
    cmd+=" -m 4096"
    cmd+=" -nographic"
    cmd+=" -drive file=\"${image_path}\",if=none,format=raw,id=hd0"
    cmd+=" -device virtio-blk-device,drive=hd0"
    cmd+=" -netdev tap,id=net0,ifname=\"${TAP}\",script=no,downscript=no"
    cmd+=" -device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56"
    cmd+=" -kernel \"${kernel_path}\""
    cmd+=" -append \"root=/dev/vda rw console=ttyAMA0\""

    # Add USB controller and devices if specified
    if [[ ${#USB_DEVICES[@]} -gt 0 ]]
    then
        debug "Adding USB support with ${#USB_DEVICES[@]} device(s)"
        cmd+=" -device qemu-xhci,id=xhci"

        for usb_dev in "${USB_DEVICES[@]}"
        do
            # Parse vendor:product ID
            if [[ ! "${usb_dev}" =~ ^[0-9a-fA-F]{4}:[0-9a-fA-F]{4}$ ]]
            then
                error "Invalid USB device ID format: ${usb_dev}"
                echo "Expected format: VVVV:PPPP (e.g., 0781:5583)"
                echo "Run 'lsusb' to find correct IDs"
                exit 1
            fi

            IFS=':' read -r vid pid <<< "${usb_dev}"
            # Ensure 0x prefix for hex values
            vid="0x${vid#0x}"
            pid="0x${pid#0x}"

            debug "Passing through USB device ${vid}:${pid}"
            cmd+=" -device usb-host,vendorid=${vid},productid=${pid}"
        done

        info "USB passthrough enabled for ${#USB_DEVICES[@]} device(s)"
        info "See ${BLUE}docs/qemu-usb.md${NC} for troubleshooting and usage"
    fi

    if [[ "${DRY_RUN}" == "true" ]]
    then
        execute "${cmd}"
    else
        eval exec "${cmd}"
    fi
}

# Show usage
show_usage() {
    cat << EOF
Usage: $0 [options]

Launch WendyOS in QEMU with networking support.

Options:
    -v, --verbose        Enable verbose/debug output
    -n, --dry-run        Show what would be done without executing
    -h, --help           Show this help message
    -b, --build-dir DIR  Specify build directory (default: build)
    --usb VENDOR:PRODUCT Pass through USB device to guest (can be used multiple times)
                         Use 'lsusb' to find vendor:product IDs
                         Example: --usb 0781:5583 for SanDisk USB drive

Environment Variables:
    BUILD_DIR           Build directory path (default: build)

Examples:
    $0                              # Launch QEMU with default settings
    $0 --dry-run                    # Show what would be executed
    $0 --verbose                    # Launch with debug output
    $0 --build-dir ../my-build      # Use custom build directory
    BUILD_DIR=../build $0           # Use environment variable
    $0 --usb 0781:5583              # Pass through USB flash drive
    $0 --usb 046d:c52b --usb 0403:6001  # Pass through multiple USB devices
    $0 --verbose --usb 0781:5583    # USB passthrough with verbose output

Notes:
    - This script automatically sets up the network if needed
    - The guest will get an IP from 10.42.0.10-250 via DHCP
    - The host is accessible at 10.42.0.1 from the guest
    - Use Ctrl-A, then X to exit QEMU
    - USB devices require proper host permissions (see docs/qemu-usb.md)

EOF
}

# Main script logic
main() {
    local workspace_dir
    local script_dir
    local deploy_dir
    local image
    local kernel
    local image_path
    local kernel_path

    # Parse flags and arguments
    while [[ $# -gt 0 ]]
    do
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
            -b|--build-dir)
                BUILD_DIR="$2"
                shift 2
                ;;
            --usb)
                if [[ -z "$2" || "$2" == -* ]]; then
                    error "Option --usb requires a VENDOR:PRODUCT argument"
                    echo "Example: --usb 0781:5583"
                    echo "Run 'lsusb' to find device IDs"
                    exit 1
                fi
                USB_DEVICES+=("$2")
                shift 2
                ;;
            *)
                error "Unknown argument: $1"
                echo ""
                show_usage
                exit 1
                ;;
        esac
    done

    debug "Verbose mode enabled"

    if [[ "${DRY_RUN}" == "true" ]]
    then
        info "${YELLOW}Dry-run mode enabled - no actions will be taken${NC}"
        echo ""
    fi

    # Check required tools
    check_required_tools

    # Detect directories
    workspace_dir=$(detect_directories)
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

    debug "Workspace: ${workspace_dir}"
    debug "Build directory: ${BUILD_DIR}"

    # Set up paths - check tmp-qemu first, then tmp
    if [[ -d "${BUILD_DIR}/tmp-qemu/deploy/images/${MACHINE}" ]]; then
        deploy_dir="${BUILD_DIR}/tmp-qemu/deploy/images/${MACHINE}"
    else
        deploy_dir="${BUILD_DIR}/tmp/deploy/images/${MACHINE}"
    fi
    image="wendyos-image-${MACHINE}.rootfs.ext4"
    kernel="Image"

    debug "Deploy directory: ${deploy_dir}"
    debug "Image: ${image}"
    debug "Kernel: ${kernel}"

    # Check if we're in the right directory
    if [[ ! -d "${BUILD_DIR}" ]]
    then
        error "Build directory not found: ${BUILD_DIR}"
        echo ""
        echo "Please run this script from the workspace root, or set BUILD_DIR:"
        echo "  cd ${workspace_dir}"
        echo "  ./meta-edgeos/scripts/$(basename "$0")"
        echo ""
        echo "Or specify build directory:"
        echo "  $0 --build-dir /path/to/build"
        exit 1
    fi

    # Check if image exists
    if ! check_image_exists "${deploy_dir}" "${image}" "${kernel}"
    then
        exit 1
    fi

    # Get absolute paths
    image_path="$(cd "$(dirname "${deploy_dir}/${image}")" && pwd)/$(basename "${image}")"
    kernel_path="$(cd "$(dirname "${deploy_dir}/${kernel}")" && pwd)/$(basename "${kernel}")"

    debug "Image path: ${image_path}"
    debug "Kernel path: ${kernel_path}"

    # Setup network if needed
    if ! setup_network_if_needed "${script_dir}"
    then
        exit 1
    fi

    # Launch QEMU
    launch_qemu "${image_path}" "${kernel_path}"
}

main "$@"
