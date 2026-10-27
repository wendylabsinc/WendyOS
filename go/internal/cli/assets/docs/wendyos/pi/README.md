# WendyOS on Raspberry Pi

WendyOS supports **Raspberry Pi 5**, **Raspberry Pi 4**, and **Raspberry Pi 3**, with one image per board. It boots from an SD card or a USB disk on the Pi 3 and Pi 4, and from an SD card or NVMe on the Pi 5.

## Partition Layout

Raspberry Pi images use WIC (OpenEmbedded Image Creator). The partition table format depends on the board:

- **Raspberry Pi 4 and 5** use a **GPT** partition table (`rpi-partuuid.wks`). Partitions are referenced by PARTUUID in `cmdline.txt` and `fstab`.
- **Raspberry Pi 3** uses an **MBR** (msdos) partition table (`rpi-mbr.wks`). The BCM2837 GPU bootrom can only read MBR-partitioned FAT32 and cannot locate `bootcode.bin` on a GPT disk, so the board never boots from a GPT image. Partitions are referenced by filesystem label instead of PARTUUID.

### SD card layout

| Partition | Label | Size | FS | Mount |
|---|---|---|---|---|
| boot | boot | 128 MB | FAT32 | `/boot` |
| config | config | 64 MB* | FAT32 | `/config` |
| root | root | 8 GB | ext4 | `/` |

### Other boot disks

The same image and partition layout also boot from a USB disk (Pi 3 and Pi 4) or an NVMe drive (Pi 5). U-Boot tries the SD card first, so remove a WendyOS SD card to boot the other disk.

\* Default size. See [Build configuration](#build-configuration).

## Config partition

See [Config Partition](../config-partition.md) for full details and usage guidance.

## Build configuration

| Variable | Default | Description |
|---|---|---|
| `WENDYOS_CONFIG_PART_SIZE_MB` | `64` | Size of the FAT32 config partition in MB |

Set this in your `local.conf` or distro config to override the default.
