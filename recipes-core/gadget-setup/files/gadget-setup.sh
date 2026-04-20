#!/bin/bash

# USB Gadget Setup for WendyOS
# Configures a composite NCM+ACM USB gadget with optimized settings
# Supports: Jetson (tegra-xudc), RPi5 (dwc2), and other Linux USB controllers
#

exec 1> >(logger -s -t "$(basename "$0")") 2> >(logger -s -t "$(basename "$0")" -p daemon.err)
set -euo pipefail

log_info()    { logger -s -t "$(basename "$0")" -p daemon.info    "$1"; }
log_warning() { logger -s -t "$(basename "$0")" -p daemon.warning "$1"; }
log_error()   { logger -s -t "$(basename "$0")" -p daemon.err     "$1"; }

# Generate a locally-administered MAC from an arbitrary string (sha256)
generate_mac() {
    local hash
    hash=$(echo -n "$1" | sha256sum | awk '{print $1}')
    local mac_base=${hash:0:12}
    local first_byte=${mac_base:0:2}
    local second_char
    second_char=$(printf '%x' $((0x$first_byte & 0xfe | 0x02)))
    printf "%02x:%02x:%02x:%02x:%02x:%02x" \
        0x$second_char \
        0x${mac_base:2:2} \
        0x${mac_base:4:2} \
        0x${mac_base:6:2} \
        0x${mac_base:8:2} \
        0x${mac_base:10:2}
}

### Resolve device serial number — combined fallback chain ###

DEVICE_SERIAL=""
if [ -f /proc/device-tree/serial-number ]; then
    DEVICE_SERIAL=$(cat /proc/device-tree/serial-number 2>/dev/null | tr -d '\0') || true
fi
if [ -z "$DEVICE_SERIAL" ]; then
    DEVICE_SERIAL=$(cat /sys/devices/platform/tegra-fuse/uid 2>/dev/null || echo "")
fi
if [ -z "$DEVICE_SERIAL" ] && [ -f /proc/cpuinfo ]; then
    DEVICE_SERIAL=$(awk -F ': ' '/Serial/ {print $2}' /proc/cpuinfo 2>/dev/null) || true
fi
if [ -z "$DEVICE_SERIAL" ]; then
    DEVICE_SERIAL=$(cat /sys/class/net/eth0/address 2>/dev/null | tr -d ':' || echo "")
fi
if [ -z "$DEVICE_SERIAL" ]; then
    DEVICE_SERIAL="$(date +%s)"
fi

SHORT_SERIAL=${DEVICE_SERIAL: -8}
CLEAN_SERIAL=$(echo "$SHORT_SERIAL" | tr -d ':')

# Friendly device name (falls back to serial suffix)
USB_SERIAL=${USB_SERIAL:-$DEVICE_SERIAL}
DEVICE_NAME_FILE="/etc/wendyos/device-name"
if [ -f "$DEVICE_NAME_FILE" ]; then
    DEVICE_FRIENDLY_NAME=$(cat "$DEVICE_NAME_FILE" | tr -d '[:space:]') || true
    log_info "Using device name from $DEVICE_NAME_FILE: $DEVICE_FRIENDLY_NAME"
else
    DEVICE_FRIENDLY_NAME="${CLEAN_SERIAL}"
    log_warning "Device name file not found, using serial: $DEVICE_FRIENDLY_NAME"
fi

USB_VID=${USB_VID:-0x1d6b}          # Linux Foundation
USB_PID=${USB_PID:-0x0104}          # Multifunction Composite Gadget
USB_MFR=${USB_MFR:-"Wendy Labs Inc"}
USB_PROD=${USB_PROD:-"WendyOS Device ${DEVICE_FRIENDLY_NAME}"}
CFG_NAME=${CFG_NAME:-"CDC+ACM"}
GADGET_FUNC_ORDER="${GADGET_FUNC_ORDER:-ncm ecm}"

GADGET_NAME="wendyos_device"
GADGET_DIR="/sys/kernel/config/usb_gadget/${GADGET_NAME}"

### Detect USB controller ###

USB_VERSION="0x0200"
USB_CONTROLLER="unknown"
if lsmod | grep -q "^tegra_xudc " || find /sys/class/udc -name "*tegra*" -type l 2>/dev/null | grep -q tegra; then
    USB_VERSION="0x0320"; USB_CONTROLLER="tegra-xudc"
    log_info "Detected tegra-xudc — USB 3.2 mode"
elif lsmod | grep -q "^dwc3 " || find /sys/class/udc -name "*dwc3*" -type l 2>/dev/null | grep -q dwc3; then
    USB_VERSION="0x0300"; USB_CONTROLLER="dwc3"
    log_info "Detected dwc3 — USB 3.0 mode"
elif lsmod | grep -q "^dwc2 " || find /sys/class/udc -name "*dwc2*" -type l 2>/dev/null | grep -q dwc2; then
    USB_VERSION="0x0200"; USB_CONTROLLER="dwc2"
    log_info "Detected dwc2 — USB 2.0 mode"
else
    log_info "No specific USB controller detected — defaulting to USB 2.0"
fi
log_info "USB controller: $USB_CONTROLLER, version: $USB_VERSION"

### Configure USB Gadget ###

# Clean previous gadget if any (configfs requires strict reverse-order teardown)
if [ -d "$GADGET_DIR" ]; then
    echo "" > "$GADGET_DIR/UDC" 2>/dev/null || true
    sleep 0.5  # allow UDC driver to complete async unbind before dismantling
    # Unlink function symlinks from configs before removing anything
    find "$GADGET_DIR/configs" -mindepth 2 -maxdepth 2 -type l -exec rm {} \; 2>/dev/null || true
    # Remove function directories
    find "$GADGET_DIR/functions" -mindepth 1 -maxdepth 1 -type d -exec rmdir {} \; 2>/dev/null || true
    # Remove config string dirs, then config dirs
    find "$GADGET_DIR/configs" -mindepth 2 -maxdepth 2 -type d -exec rmdir {} \; 2>/dev/null || true
    find "$GADGET_DIR/configs" -mindepth 1 -maxdepth 1 -type d -exec rmdir {} \; 2>/dev/null || true
    # Remove gadget string dirs, then the gadget itself
    find "$GADGET_DIR/strings" -mindepth 1 -maxdepth 1 -type d -exec rmdir {} \; 2>/dev/null || true
    rmdir "$GADGET_DIR" 2>/dev/null || true
fi

# Mount configfs and load modules
mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config
depmod -a || true
modprobe -q libcomposite || true
modprobe -q u_ether || true
modprobe -q usb_f_ncm || true
modprobe -q usb_f_ecm || true
modprobe -q usb_f_acm || true

mkdir -p "$GADGET_DIR"
cd "$GADGET_DIR" || { log_error "Cannot cd to $GADGET_DIR"; exit 1; }

echo "$USB_VID" > idVendor
echo "$USB_PID" > idProduct
echo "$USB_VERSION" > bcdUSB
echo 0x0100 > bcdDevice

# All controllers use composite (IAD) class — required for NCM+ACM
echo 0xEF > bDeviceClass      # Miscellaneous Device
echo 0x02 > bDeviceSubClass   # Common Class
echo 0x01 > bDeviceProtocol   # Interface Association Descriptor

mkdir -p configs/c.1/strings/0x409

# Power attributes — controller-specific
if [ "$USB_CONTROLLER" = "dwc2" ]; then
    echo 0x80 > configs/c.1/bmAttributes  # Bus-powered
    echo 250  > configs/c.1/MaxPower
else
    echo 0xC0 > configs/c.1/bmAttributes  # Self-powered
fi

mkdir -p strings/0x409
echo "$USB_SERIAL"  > strings/0x409/serialnumber
echo "$USB_MFR"     > strings/0x409/manufacturer
echo "$USB_PROD"    > strings/0x409/product

echo "$CFG_NAME" > configs/c.1/strings/0x409/configuration

# Generate stable per-device MACs from the serial
mac_host=$(generate_mac "${DEVICE_SERIAL}-host")
mac_self=$(generate_mac "${DEVICE_SERIAL}-dev")
log_info "NCM MACs — host: $mac_host  self: $mac_self"

add_func() {
    case "$1" in
    ncm)
        mkdir -p functions/ncm.usb0 2>/dev/null || return 1
        echo "$mac_host" > functions/ncm.usb0/host_addr 2>/dev/null || true
        echo "$mac_self" > functions/ncm.usb0/dev_addr  2>/dev/null || true
        echo 10          > functions/ncm.usb0/qmult     2>/dev/null || true
        ln -sf functions/ncm.usb0 configs/c.1/
        return 0
        ;;
    ecm)
        mkdir -p functions/ecm.usb0 2>/dev/null || return 1
        echo "$mac_host" > functions/ecm.usb0/host_addr 2>/dev/null || true
        echo "$mac_self" > functions/ecm.usb0/dev_addr  2>/dev/null || true
        ln -sf functions/ecm.usb0 configs/c.1/
        return 0
        ;;
    esac
}

SEL=""
for f in $GADGET_FUNC_ORDER; do
    if add_func "$f"; then
        SEL="$f"; break
    fi
done

[ -n "$SEL" ] || { log_error "Neither NCM nor ECM is available"; exit 1; }
log_info "Network function selected: $SEL"

# ACM serial console function (creates /dev/ttyGS0 on device, ttyACM0 on host)
mkdir -p functions/acm.usb0
ln -sf functions/acm.usb0 configs/c.1/
log_info "ACM serial function configured (/dev/ttyGS0)"

# Wait for a UDC to appear (up to 60 s)
UDC=""
for i in $(seq 60); do
    UDC=$(ls /sys/class/udc 2>/dev/null | head -n1) || true
    if [ -n "$UDC" ]; then
        break
    fi
    sleep 1
done
[ -n "$UDC" ] || { log_error "UDC timeout after 60 s"; exit 1; }
log_info "Found UDC: $UDC"

# Clear any previous binding before activating
if [ -f UDC ] && [ -s UDC ]; then
    echo "" > UDC 2>/dev/null || true
    sleep 1
fi
echo "$UDC" > UDC
log_info "UDC activated: $UDC"

# On tegra-xudc the UDC relies on a usb_phy notifier chain fed by the
# padctl role switch (phy-tegra-xusb → fusb301 → tegra_xudc).  If that
# chain has not fired yet (e.g. FUSB301 probed before padctl ports were
# ready, or cable was connected before boot), the UDC sits in "default"
# state and never enters device mode.  Force the role explicitly so the
# usb_phy notifier fires and XUDC enters device mode regardless of the
# CC-chip state machine.  The padctl exposes the role switch at:
#   /sys/class/usb_role/usb2-0-role-switch/role
# (name comes from dev_set_name(&port->dev, "usb2-0") in xusb.c and
#  dev_set_name(&sw->dev, "%s-role-switch", dev_name(parent)) in roles/class.c)
if [ "$USB_CONTROLLER" = "tegra-xudc" ]; then
    ROLE_SW_PATH="/sys/class/usb_role/usb2-0-role-switch/role"
    if [ -f "$ROLE_SW_PATH" ]; then
        echo "device" > "$ROLE_SW_PATH" 2>/dev/null && \
            log_info "Forced USB role switch to device" || \
            log_warning "Failed to force USB role switch to device"
    else
        log_warning "tegra-xudc: USB role switch not found at $ROLE_SW_PATH — XUDC may stay in default state"
    fi
fi

udevadm settle -t 20 || log_warning "udevadm settle timed out — gadget may not be fully enumerated"
log_info "USB gadget initialised"

# Interface performance tuning
ip link set usb0 txqueuelen 2000 2>/dev/null || true

# Pin USB IRQ to CPUs 0-3 for cache locality
# Covers Jetson (3550000.usb / tegra-xudc), RPi3 (3f980000.usb),
# RPi4 (fe980000.usb), and RPi5 (1000480000.usb via RP1)
USB_IRQ=$(grep -E "3550000\.usb|3f980000\.usb|fe980000\.usb|1000480000\.usb" /proc/interrupts 2>/dev/null \
          | cut -d: -f1 | tr -d ' ' | head -1) || true
if [ -n "$USB_IRQ" ]; then
    echo "0f" > /proc/irq/"$USB_IRQ"/smp_affinity 2>/dev/null || true
    log_info "USB IRQ $USB_IRQ pinned to CPUs 0-3"
fi

log_info "Gadget ready: $SEL+ACM on usb0, UDC=$UDC, USB=$USB_VERSION — NetworkManager will configure network settings"
