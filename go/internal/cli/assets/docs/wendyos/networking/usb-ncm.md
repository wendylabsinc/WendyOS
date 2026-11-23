# USB NCM Gadget

WendyOS configures the device as a composite USB gadget — a single USB device that exposes both a network interface (NCM) and a serial console (ACM) to the host. This is done at boot time using the Linux USB gadget framework via configfs.

## What Is USB NCM?

NCM (Network Control Model) is a USB class protocol that carries Ethernet frames over USB. It is part of the CDC (Communications Device Class) family. Compared to the older ECM protocol, NCM offers better throughput and is the preferred mode. WendyOS falls back to ECM only if the kernel does not have NCM support compiled in.

On the host, the gadget appears as a standard Ethernet adapter. Linux hosts load the `cdc_ncm` driver automatically; macOS supports NCM natively; Windows requires a driver for older OS versions.

## Composite Gadget Layout

The gadget exposes two functions in a single configuration:

```
USB Configuration c.1 (CDC+ACM)
├── functions/ncm.usb0   → network interface (usb0 on device, enxXXXXXXXXXXXX on host)
└── functions/acm.usb0   → serial console   (/dev/ttyGS0 on device, /dev/ttyACM0 on host)
```

USB descriptor strings set on the gadget:
- **Manufacturer**: `Wendy Labs Inc`
- **Product**: `WendyOS <device-name>` (`WendyOS Device <device-name>` on images up to 0.21)
- **Serial number**: device serial (from device tree, tegra fuse, `/proc/cpuinfo`, or `eth0` MAC)

USB IDs:
- **VID**: `0x1d6b` (Linux Foundation)
- **PID**: `0x0104` (Multifunction Composite Gadget)

## Kernel Configuration

The gadget framework requires these kernel options. On RPi5 they are built-in; on Jetson they are loaded as modules.

**RPi5 (`meta-rpi-extensions/recipes-kernel/linux/linux-raspberrypi/usb-gadget.cfg`):**
```
CONFIG_CONFIGFS_FS=y
CONFIG_USB_GADGET=y
CONFIG_USB_LIBCOMPOSITE=y
CONFIG_USB_CONFIGFS=y
CONFIG_USB_CONFIGFS_ACM=y
CONFIG_USB_CONFIGFS_NCM=y
CONFIG_USB_CONFIGFS_ECM=y
CONFIG_USB_F_ACM=y
CONFIG_USB_F_NCM=y
CONFIG_USB_F_ECM=y
```

**Jetson (`meta-tegra-extensions/...usb-gadget.cfg`):** Same options but as modules (`=m`). The module load order matters — `tegra_xudc` must load before `libcomposite` so that the UDC is registered before `gadget-setup.sh` runs.

## Initialization Flow

`gadget-setup.service` runs `gadget-setup.sh` once at boot, before NetworkManager starts.

### Step 1 — Resolve device serial number

The script tries these sources in order, stopping at the first non-empty result:

1. `/proc/device-tree/serial-number` (RPi device tree)
2. `/sys/devices/platform/tegra-fuse/uid` (Jetson eFuse UID)
3. `awk '/Serial/ {print $2}' /proc/cpuinfo` (legacy RPi CPU info)
4. `eth0` MAC address (stripped of colons)
5. Unix timestamp (last-resort fallback)

The last 8 characters are extracted as `SHORT_SERIAL` and used for the human-readable device name when no `/etc/wendyos/device-name` file is present.

### Step 2 — Detect USB controller

```bash
if lsmod | grep -q "^tegra_xudc"; then  USB_VERSION=0x0320  # USB 3.2
elif lsmod | grep -q "^dwc3";     then  USB_VERSION=0x0300  # USB 3.0
elif lsmod | grep -q "^dwc2";     then  USB_VERSION=0x0200  # USB 2.0
fi
```

The bcdUSB descriptor is set accordingly. The bus power attribute also changes: `dwc2` devices set `bmAttributes=0x80` (bus-powered, 250 mA), while other controllers use `0xC0` (self-powered).

### Step 3 — Generate stable MAC addresses

Two MAC addresses are derived deterministically from the device serial. See [MAC Address Generation](./mac-addresses.md) for the algorithm.

### Step 4 — Build the configfs tree

```bash
GADGET_DIR=/sys/kernel/config/usb_gadget/wendyos_device
mkdir -p $GADGET_DIR
echo 0x1d6b > idVendor
echo 0x0104 > idProduct
echo $USB_VERSION > bcdUSB
echo 0xEF > bDeviceClass    # Miscellaneous (required for IAD/composite)
echo 0x02 > bDeviceSubClass
echo 0x01 > bDeviceProtocol
# ... string descriptors ...
mkdir -p functions/ncm.usb0
echo $mac_host > functions/ncm.usb0/host_addr
echo $mac_self > functions/ncm.usb0/dev_addr
echo 10       > functions/ncm.usb0/qmult      # queue multiplier for throughput
ln -sf functions/ncm.usb0 configs/c.1/
mkdir -p functions/acm.usb0
ln -sf functions/acm.usb0 configs/c.1/
```

NCM falls back to ECM if `functions/ncm.usb0` cannot be created (kernel without `usb_f_ncm`).

### Step 5 — Activate the UDC

The script polls `/sys/class/udc` for up to 60 seconds until a UDC appears, then writes its name to `$GADGET_DIR/UDC` to bind the gadget to the hardware controller.

### Step 6 — Tegra role-switch workaround

On Jetson, the USB-C port role is controlled by a CC chip (FUSB301). If the padctl role-switch notifier chain has not fired by the time the gadget is activated, the UDC stays in "default" mode and never enters device mode. The script forces the role explicitly:

```bash
echo "device" > /sys/class/usb_role/usb2-0-role-switch/role
```

It falls back to a glob (`usb2-*-role-switch`) on AGX-style topologies where the index differs.

### Step 7 — Post-activation tuning

After the UDC binds:
- `usb0-force-up` brings `usb0` administratively up (retries for up to 10 seconds to avoid races).
- `ip link set usb0 txqueuelen 2000` increases the TX queue length.
- The USB interrupt is pinned to CPUs 0–3 via `/proc/irq/<irq>/smp_affinity`.

## systemd Services

| Service | Purpose |
|---|---|
| `gadget-setup.service` | Runs `gadget-setup.sh` + `usb0-force-up`; type `oneshot`, `RemainAfterExit=yes` |
| `wendyos-usbgadget-unbind.service` | Triggered by udev `udc remove` event; writes `""` to the UDC file to cleanly unbind |

**Service ordering:** `gadget-setup.service` runs `After=sysinit.target local-fs.target systemd-udevd.service` and `Before=NetworkManager.service network.target`.

## udev Rules

| Rule file | Effect |
|---|---|
| `90-usb0-up.rules` | Runs `usb0-force-up` whenever `usb0` is added or changes state |
| `99-usb-gadget-udc.rules` | Triggers `wendyos-usbgadget-unbind.service` on UDC removal |

## Teardown (Rebinding)

The script handles re-runs gracefully. If `$GADGET_DIR` already exists it performs a strict reverse-order teardown:
1. Clear UDC binding
2. Remove function symlinks from configs
3. Remove function directories
4. Remove config string and config directories
5. Remove gadget string directories and the gadget root

This avoids the EBUSY errors that occur when configfs entries are removed out of order.

## Known Issues

**RPi5 / DWC2 DMA stall (kernel 6.6, BCM2712):** BCM2712 reports DMA capability but DMA completion interrupts never fire for Out (host to device) transfers in peripheral mode. The symptom is that the device can send data but receives nothing from the host. The fix is `g_dma=false` in `dwc2_set_bcm_params()`. WendyOS applies this in a kernel patch under `meta-rpi-extensions/`. Do not write `""` to the UDC sysfs file on an affected board without this patch — doing so triggers `ep_stop_xfr` timeouts that leave Global OUT NAK permanently asserted until reboot.
