# U-Boot and Boot Configuration

WendyOS on Raspberry Pi 3, 4 and 5 boots through U-Boot by default: the shared Raspberry Pi machine settings set `WENDYOS_RPI_UBOOT ?= "1"`. The RPi firmware reads `config.txt` and `cmdline.txt` from the FAT32 `/boot` partition and starts U-Boot instead of the kernel. U-Boot runs `boot.scr`, which on the default `wendyos-update` stack (`WENDYOS_OTA = "wendy"`) picks the A/B rootfs slot from `uboot.env`, loads the kernel from that slot's `/boot` directory, and adds `root=` for the slot to the kernel command line.

The default is the same on every Raspberry Pi board but depends on the build configuration. Set `WENDYOS_RPI_UBOOT = "0"` to build without U-Boot; the firmware then loads the kernel directly from the `/boot` partition. With a `WENDYOS_OTA` value other than `"wendy"`, `boot.scr` is the stock meta-raspberrypi single-rootfs script.

This document covers that boot configuration: where the files live, what WendyOS sets, and how to customise them.

## Boot files on the `/boot` partition

| File | Purpose |
|------|---------|
| `config.txt` | RPi firmware configuration (hardware init, overlays, UART, PCIe, GPU) |
| `cmdline.txt` | Linux kernel command line (consoles, init parameters); with U-Boot, `boot.scr` adds the root device |
| `bcm*.dtb` | Device Tree Blob for the SoC |
| `overlays/` | Device Tree overlays referenced by `config.txt` |
| `kernel8.img` (or `kernel_2712.img` on RPi 5) | U-Boot (`u-boot.bin`), stored under the file name the firmware loads as its kernel. Without U-Boot, the kernel image itself |
| `boot.scr` | U-Boot boot script: selects the A/B rootfs slot and loads its kernel |
| `uboot.env` | U-Boot environment, including the selected slot (`wendyos_boot_slot`) |
| `initrd.img` | Initramfs (if present) |

## `config.txt`

WendyOS sets the following in `config.txt` via the `rpi-config_%.bbappend` recipe:

```ini
# UART
enable_uart=1

# RPi 4: standard uart0 overlay
dtoverlay=uart0

# RPi 5: Pi5-specific overlay (uart0-pi5)
dtoverlay=uart0-pi5

# USB gadget mode (added when ENABLE_DWC2_PERIPHERAL=1)
dtoverlay=dwc2

# PCIe x1 lane (RPi 5 only, for NVMe access)
dtparam=pciex1
```

Additional machine-level settings (from `raspberrypi5-wendyos.conf` / `raspberrypi4-64-wendyos.conf`):

| Setting | Value | Description |
|---------|-------|-------------|
| `DISABLE_RPI_BOOT_LOGO` | `1` | Suppresses the rainbow splash |
| `DISABLE_SPLASH` | `1` | No boot splash |
| `DISABLE_OVERSCAN` | `1` | No overscan compensation |
| `ENABLE_UART` | `1` | Enables primary UART |
| GPU memory | `16 MB` | Headless mode (set via meta-raspberrypi defaults) |

You can add your own entries to `config.txt` in two ways:

1. **At build time** — set `RPI_EXTRA_CONFIG` in `local.conf`:
   ```
   RPI_EXTRA_CONFIG = "dtparam=i2c_arm=on\n"
   ```
2. **At runtime** — mount the `/boot` partition and edit `config.txt` directly, then reboot.

## `cmdline.txt`

The kernel command line is assembled by meta-raspberrypi from several sources. WendyOS appends:

```
console=tty1
```

With U-Boot (the default), `boot.scr` takes the firmware's command line and appends `root=` for the selected A/B slot, followed by `rootwait rw`. Because it comes last, it overrides any `root=` in `cmdline.txt`. The root device depends on the board:

- **RPi 4 and 5** (GPT layout): slot A is partition 3 and slot B is partition 4, for example `root=/dev/mmcblk0p3` on an SD card or `root=/dev/nvme0n1p3` on the RPi 5 NVMe image. When an RPi 4 boots the image from another disk, such as USB, `boot.scr` passes the slot's `root=PARTUUID=<uuid>` instead.
- **RPi 3** (MBR layout): slot A is partition 2 and slot B is partition 3, for example `root=/dev/mmcblk0p2`.

Run `cat /proc/cmdline` on the device to see the full command line it booted with.

| Parameter | Description |
|-----------|-------------|
| `console=serial0,115200` | Hardware UART console (alias for the model-appropriate UART device). See [uart.md](uart.md). |
| `console=tty1` | HDMI framebuffer console added by WendyOS |
| `root=/dev/mmcblk0p<N>` | Selected A/B slot, appended by `boot.scr` (`/dev/nvme0n1p<N>` on the RPi 5 NVMe image) |
| `root=PARTUUID=<uuid>` | Selected A/B slot when an RPi 4 boots the image from a disk other than the one it was built for |
| `rw` | Mount the root filesystem read-write; the A/B `fstab` has no `/` entry |
| `rootfstype=ext4` | Root filesystem type |
| `rootwait` | Wait for the block device to become available |

To add additional kernel parameters at build time, set `CMDLINE:append` in your layer or `local.conf`:

```bitbake
CMDLINE:append = " your.param=value"
```

## Boot order (RPi 5 EEPROM)

On RPi 5, boot order and PCIe probe are stored in the EEPROM, not in `config.txt`. WendyOS manages two EEPROM settings automatically via one-shot systemd services:

| Setting | Value | Service |
|---------|-------|---------|
| `PSU_MAX_CURRENT` | `3000` | `rpi5-eeprom-config.service` |
| `PCIE_PROBE` | `1` | `rpi5-eeprom-nvme-config.service` |
| `BOOT_ORDER` | `0xf461` | `rpi5-eeprom-nvme-config.service` |

`BOOT_ORDER=0xf461` means (right-to-left nibbles): SD card (1) → NVMe (6) → Network (4) → restart (f).

Both services are guarded by flag files (`/var/lib/wendyos/eeprom-updated`, `/var/lib/wendyos/eeprom-nvme-updated`) so they run at most once per device. If an update is needed, the service stages it with `rpi-eeprom-update` and reboots.

See [eeprom.md](eeprom.md) for full details on reading and writing EEPROM settings.

## Reading current boot configuration

```sh
# View current config.txt
cat /boot/config.txt

# View kernel command line that was used for this boot
cat /proc/cmdline

# RPi 5: read current EEPROM configuration
rpi-eeprom-config
```

## Customising at runtime

The `/boot` partition is writable. After editing, reboot to pick up changes.

```sh
# Remount read-write if necessary
mount -o remount,rw /boot

# Edit
nano /boot/config.txt

# Sync and reboot
sync && reboot
```

> Note: OTA updates use `wendyos-update`, which writes the new rootfs to the inactive A/B slot. On Raspberry Pi, the `/boot` and `/config` partitions sit outside the A/B slots, so an update does not replace your edits to them.
