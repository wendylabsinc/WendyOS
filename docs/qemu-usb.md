# USB Device Passthrough in QEMU - WendyOS Guide

## Overview

This document describes how to attach USB devices from the host to the QEMU virtual machine running WendyOS. USB passthrough allows the guest OS to directly access physical USB devices connected to the host.

## Prerequisites

### Host System Setup

1. **User Permissions**
   ```bash
   # Add your user to plugdev group
   sudo usermod -a -G plugdev $USER

   # Log out and back in for changes to take effect
   ```

2. **udev Rules (Recommended)**

   Create a device-specific rule in `/etc/udev/rules.d/`:
   ```bash
   # Replace VVVV:PPPP with your device's vendor:product ID from lsusb
   echo 'SUBSYSTEM=="usb", ATTRS{idVendor}=="VVVV", ATTRS{idProduct}=="PPPP", MODE="0664", GROUP="plugdev"' \
     | sudo tee /etc/udev/rules.d/99-qemu-usb-VVVV-PPPP.rules
   ```

   Example for A-DATA USB Flash Drive (`125f:dd35`):
   ```bash
   echo 'SUBSYSTEM=="usb", ATTRS{idVendor}=="125f", ATTRS{idProduct}=="dd35", MODE="0664", GROUP="plugdev"' \
     | sudo tee /etc/udev/rules.d/99-qemu-usb-125f-dd35.rules
   ```

   Apply rules (re-plug the device after this):
   ```bash
   sudo udevadm control --reload-rules && sudo udevadm trigger
   ```

   Verify it worked:
   ```bash
   # Find bus/device numbers from lsusb, then check permissions
   ls -la /dev/bus/usb/BUS/DEV
   # Should show: crw-rw-r-- 1 root plugdev ...
   ```

## Finding USB Devices

### List All USB Devices

```bash
# Simple list
lsusb

# Example output:
# Bus 001 Device 004: ID 046d:c52b Logitech, Inc. Unifying Receiver
# Bus 001 Device 005: ID 0781:5583 SanDisk Corp. Ultra Fit

# Detailed information
lsusb -v -d 046d:c52b

# Check device permissions
ls -l /dev/bus/usb/001/004
```

### Understanding USB Device IDs

Format: `Bus XXX Device YYY: ID VVVV:PPPP Vendor Product`

- **VVVV**: Vendor ID (4 hex digits)
- **PPPP**: Product ID (4 hex digits)
- **Bus/Device**: Physical USB bus and device number (changes on replug)

**Recommendation**: Use Vendor:Product ID for passthrough (survives device reconnection)

## Using run-qemu.sh with USB Devices

The `run-qemu.sh` script supports USB passthrough via the `--usb` option.

### Basic Usage

```bash
# Pass through a single USB device
./scripts/run-qemu.sh --usb 0781:5583

# Pass through multiple devices
./scripts/run-qemu.sh --usb 046d:c52b --usb 0403:6001

# Combined with other options
./scripts/run-qemu.sh --verbose --usb 0781:5583
```

### Common Use Cases

#### 1. USB Storage Device (Flash Drive)

```bash
# Find device
lsusb | grep -i sandisk
# Output: Bus 001 Device 005: ID 0781:5583 SanDisk Corp. Ultra Fit

# Run QEMU
./scripts/run-qemu.sh --usb 0781:5583

# In guest:
lsblk                    # Should show the USB drive (e.g., sdb)
sudo mount /dev/sdb1 /mnt
```

#### 2. USB Serial Device (Arduino, FTDI, GPS)

```bash
# Find device
lsusb | grep -i "FTDI\|Serial\|Arduino"
# Output: Bus 001 Device 006: ID 0403:6001 FTDI USB-Serial

# Run QEMU
./scripts/run-qemu.sh --usb 0403:6001

# In guest:
ls /dev/ttyUSB*          # Device appears as /dev/ttyUSB0
sudo minicom -D /dev/ttyUSB0
```

#### 3. USB Webcam

```bash
# Find device
lsusb | grep -i camera
# Output: Bus 001 Device 007: ID 046d:0825 Logitech Webcam

# Run QEMU
./scripts/run-qemu.sh --usb 046d:0825

# In guest:
ls /dev/video*           # Should show /dev/video0
v4l2-ctl --list-devices  # List video devices
```

#### 4. USB-to-Ethernet Adapter

```bash
# Find device
lsusb | grep -i "Ethernet\|ASIX\|Realtek"

# Run QEMU
./scripts/run-qemu.sh --usb 0b95:1790

# In guest:
ip link show             # New interface appears (e.g., eth1)
```

## USB Controller Types

The script automatically uses USB 3.0 (XHCI) controller when USB devices are specified:

- **USB 1.1 (UHCI)**: Legacy, low-speed devices
- **USB 2.0 (EHCI)**: Most common devices, 480 Mbps
- **USB 3.0 (XHCI)**: High-speed devices, 5 Gbps (used by run-qemu.sh)

## Troubleshooting

### Device Not Appearing in Guest

**Symptoms**: USB device doesn't show up in `lsusb` inside guest

**Solutions**:
```bash
# 1. Verify host can see device
lsusb -v -d 0781:5583

# 2. Check permissions on host
ls -l /dev/bus/usb/001/005
# Should show rw-rw-rw- or your user has access

# 3. Check if device is bound to host driver
lsusb -t  # Shows driver bindings

# 4. Inside guest, check kernel messages
dmesg | tail -30
```

### "Device or resource busy" Error

**Cause**: Device is claimed by a host driver

**Solutions**:
```bash
# Option 1: Unbind from host driver
# Find the USB device path
ls /sys/bus/usb/devices/
# Example: 1-5 for Bus 1, Port 5

# Unbind
echo "1-5" | sudo tee /sys/bus/usb/drivers/usb/unbind

# Option 2: Blacklist the host driver
# Add to /etc/modprobe.d/blacklist-usb.conf:
blacklist usb_storage    # Example: for flash drives
```

### Permission Denied Errors

**Cause**: The USB device node is owned by `root:root` with no write access for other users.
You can confirm this with:
```bash
ls -la /dev/bus/usb/BUS/DEV
# crw-rw-r-- 1 root root ...   <-- QEMU cannot open for write, device won't appear in guest
```

**Solutions**:
```bash
# 1. Check group membership
groups | grep plugdev

# 2. Add yourself to plugdev group (if not already a member)
sudo usermod -aG plugdev $USER
# Then log out and back in

# 3. Create a permanent udev rule (recommended)
echo 'SUBSYSTEM=="usb", ATTRS{idVendor}=="VVVV", ATTRS{idProduct}=="PPPP", MODE="0664", GROUP="plugdev"' \
  | sudo tee /etc/udev/rules.d/99-qemu-usb-VVVV-PPPP.rules
sudo udevadm control --reload-rules && sudo udevadm trigger
# Re-plug the USB device, then verify: ls -la /dev/bus/usb/BUS/DEV
# Should now show: crw-rw-r-- 1 root plugdev ...

# 4. One-off fix without udev rule (does not survive replug)
sudo ./scripts/run-qemu.sh --usb VVVV:PPPP
```

### Device Not Working After Replug

**Cause**: Using Bus/Device numbers instead of Vendor:Product IDs

**Solution**: Always use Vendor:Product format:
```bash
# Good (survives replug):
./scripts/run-qemu.sh --usb 0781:5583

# Bad (breaks on replug):
# Using hostbus/hostaddr (not supported by script, by design)
```

### Performance Issues with Storage

**Symptoms**: Slow USB storage performance

**Solutions**:
1. Ensure USB 3.0 device is used
2. Check host USB port is USB 3.0
3. For large data transfers, consider alternatives:
   - Use network file sharing (NFS/SMB)
   - Use QEMU disk images instead
   - Use virtio-blk for better performance

## Advanced Usage

### Hot-Plugging Devices (QEMU Monitor)

While QEMU is running, you can add/remove USB devices via the monitor:

```bash
# Enter QEMU monitor: Ctrl-A, then C

# List available USB host devices
(qemu) info usbhost

# Add device
(qemu) device_add usb-host,vendorid=0x046d,productid=0xc52b,id=mouse1

# List connected USB devices
(qemu) info usb

# Remove device
(qemu) device_del mouse1

# Return to console: Ctrl-A, then C again
```

### Manual QEMU Commands (for debugging)

If you need to run QEMU manually with USB:

```bash
qemu-system-aarch64 \
    -machine virt \
    -cpu cortex-a57 \
    -m 4096 \
    -device qemu-xhci,id=xhci \
    -device usb-host,vendorid=0x0781,productid=0x5583 \
    ... other options ...
```

## Security Considerations

### Risks

1. **Direct Hardware Access**: Guest has full control over the USB device
2. **DMA Attacks**: Malicious USB devices could potentially access host memory
3. **Data Exfiltration**: Guest can read data from USB devices
4. **Device Firmware**: Guest could potentially update device firmware

### Best Practices

1. **Only pass through trusted devices**
2. **Don't pass through critical devices** (keyboard/mouse if you need host control)
3. **Use dedicated USB hubs** for guest devices when possible
4. **Monitor device activity** on the host
5. **Consider USB/IP** for better isolation in production environments

## Limitations

1. **No USB hub passthrough**: Can only pass through individual devices
2. **Some devices may not work**: Devices with strict timing requirements
3. **USB boot not supported**: Cannot boot guest from USB device
4. **isochronous transfers**: May have issues with audio/video streaming devices
5. **Device resets**: Some devices don't handle guest resets well

## Alternative: USB/IP (Network USB)

For production or when better isolation is needed:

```bash
# On host (USB server):
sudo modprobe usbip-host
usbipd -D

# List devices
usbip list -l

# Bind device
sudo usbip bind -b 1-5

# On guest (USB client):
sudo modprobe vhci-hcd
sudo usbip attach -r <host-ip> -b 1-5
```

## References

- QEMU USB Documentation: https://www.qemu.org/docs/master/system/devices/usb.html
- Linux USB/IP: https://usbip.sourceforge.net/
- udev Rules Guide: https://wiki.archlinux.org/title/Udev

## Support

For issues specific to WendyOS QEMU USB passthrough:
1. Check this documentation
2. Verify prerequisites are met
3. Test with `--verbose` flag for detailed output
4. Check host and guest kernel logs (`dmesg`)
