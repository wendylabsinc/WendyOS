# WendyOS

This repository provides the meta-layer and build flow to build **WendyOS** — a Yocto-based embedded Linux distribution — for:
- **NVIDIA Jetson** Developer Kits (Orin Nano 8GB, AGX Orin 64GB)
- **Raspberry Pi 5** (SD card and NVMe boot)
- **QEMU ARM64** (virtual machine, for development)

### Supported Hardware

| Hardware | SoC | RAM | Machine Config | Boot Device |
|----------|-----|-----|----------------|-------------|
| Jetson Orin Nano DevKit | Tegra234 | 8GB | `jetson-orin-nano-devkit-wendyos` | SD |
| Jetson Orin Nano DevKit | Tegra234 | 8GB | `jetson-orin-nano-devkit-nvme-wendyos` | NVMe |
| Jetson AGX Orin DevKit | Tegra234 | 64GB | `jetson-agx-orin-devkit-nvme-wendyos` | NVMe |
| Raspberry Pi 5 | Broadcom BCM2712 | 8GB | `raspberrypi5-wendyos` | SD |
| Raspberry Pi 5 | Broadcom BCM2712 | 8GB | `raspberrypi5-nvme-wendyos` | NVMe |

## TL;DR

```bash
git clone git@github.com:wendylabsinc/meta-wendyos-jetson.git
cd meta-wendyos-jetson
make setup              # First-time setup (~10 min)
make build              # Build the image (~2-4 hours first time, uses cache after)
make flash-to-external  # Flash to external NVMe/USB drive
```
## Table of Contents

- [Quick Start](#quick-start)
  - [Prerequisites](#prerequisites)
  - [Directory Structure Requirements](#directory-structure-requirements)
  - [Steps to Build](#steps-to-build)
  - [Flash the SD Card or NVMe](#flash-the-sd-card-or-nvme)
    - [For SD Card Builds](#for-sd-card-builds)
    - [For NVMe Builds](#for-nvme-builds)
    - [Flashing the .img File](#flashing-the-img-file)
    - [Alternative: Flashing with initrd-flash (USB Recovery Mode)](#alternative-flashing-with-initrd-flash-usb-recovery-mode)
  - [Available Images](#available-images)
- [USB Gadget Networking](#usb-gadget-networking)
- [Mender OTA Updates](#mender-ota-updates)
  - [Partition Layout](#partition-layout)
  - [Manual Update](#manual-update)
  - [Mender Server Update](#mender-server-update)
    - [Setting Up Mender Server](#setting-up-mender-server)
    - [Device Configuration](#device-configuration)
    - [Deploy an Update](#deploy-an-update)
    - [Mender Configuration](#mender-configuration)
    - [Tear Down Server](#tear-down-server)
- [Advanced Configuration](#advanced-configuration)
  - [Custom Variables in bootstrap.sh](#custom-variables-in-bootstrapsh)
  - [Build Configuration Variables](#build-configuration-variables)
  - [Runtime Identity](#runtime-identity)
  - [Per-Board Repo Overrides](#per-board-repo-overrides)
- [Raspberry Pi 5](#raspberry-pi-5)
  - [Supported Machines](#supported-machines)
  - [Build](#build)
  - [Flash the Image](#flash-the-image)
- [QEMU (ARM64)](#qemu-arm64)
  - [Prerequisites](#qemu-prerequisites)
  - [Build](#qemu-build)
  - [Run](#run)
  - [Networking](#networking)
  - [Cleanup](#cleanup)
- [Architecture Notes](#architecture-notes)
- [License](#license)

## Quick Start

### Prerequisites

**Common Requirements:**
- Docker installed and running
- Git
- At least 100GB of free disk space
- Reliable internet connection

**Linux-specific:**
- The user under which the image is built must be added to `docker` group:
  ```bash
  $ sudo usermod -aG docker $USER
  ```

**macOS-specific:**
- [Docker Desktop for Mac](https://www.docker.com/products/docker-desktop/) (version 4.0+ recommended)
- Allocate sufficient resources in Docker Desktop settings:
  - **Memory**: 8GB minimum (16GB+ recommended)
  - **Disk**: 150GB minimum for build artifacts
  - **CPUs**: 4+ cores recommended
- Install GNU coreutils (optional, for older macOS versions):
  ```bash
  $ brew install coreutils
  ```

> **Note for macOS users**: The Yocto build runs inside a Docker container (Ubuntu 24.04 LTS), so macOS hosts can build just like Linux hosts. The build scripts automatically detect macOS and adjust Docker arguments accordingly.

### Directory Structure Requirements

**Important**:
The meta layer repository must be located within the working directory where you run the bootstrap script. The bootstrap creates a Docker container that mounts the working directory, so the meta-layer must be accessible within that mount.

Recommended structure:
```
/path/to/project           <- run bootstrap.sh from this folder
  +-- meta-wendyos           <- wendy meta layer repository
  +-- repos                  <- created by bootstrap (Yocto layers)
  +-- build                  <- created by bootstrap (build output)
  +-- docker                 <- created by bootstrap (Docker config)
```

### Steps to Build

#### Option A: Using Make (Recommended)

The easiest way to build is using the provided Makefile:

```bash
# Clone and enter the repository
cd /path/to/project
git clone git@github.com:wendylabsinc/meta-wendyos-jetson.git meta-wendyos
cd meta-wendyos

# First-time setup (clones repos, creates Docker image)
make setup

# Build the image
make build

# Or open an interactive shell for development
make shell
```

**Available Make Targets:**
| Target | Description |
|--------|-------------|
| `make setup` | First-time setup: clone repos, create Docker image |
| `make build` | Build the complete WendyOS image |
| `make deploy` | Copy tegraflash tarball from Docker volume to `./deploy/` (macOS only) |
| `make flash-to-external` | Interactive flash to external NVMe/USB drive (macOS & Linux) |
| `make build-sdk` | Build the SDK for application development |
| `make shell` | Open interactive shell in build container |
| `make clean` | Remove build artifacts (keeps downloads/sstate) |
| `make distclean` | Remove everything including downloads |
| `make help` | Show all available targets |

**Build for different targets:**
```bash
# Jetson Orin Nano (NVMe)
make setup BOARD=jetson-orin-nano-nvme
make build

# Jetson Orin Nano (SD card)
make setup BOARD=jetson-orin-nano-sd
make build

# Jetson AGX Orin (NVMe)
make setup BOARD=jetson-agx-orin
make build

# Raspberry Pi 5 (SD card)
make setup BOARD=rpi5-sd
make build

# Raspberry Pi 5 (NVMe)
make setup BOARD=rpi5-nvme
make build

# QEMU (ARM64, for development)
make setup BOARD=qemu-arm64
make build
```

> `BOARD` must be set to a board id matching a directory
> `conf/template/boards/<board-id>/`. There is no silent default — pick the
> correct board id up front. Each board directory contains `local.conf` and
> `bblayers.conf`, which pull in shared fragments from
> `conf/template/include/{local,bblayers}/` via BitBake `require`.
>
> `MACHINE=<board-id>` still works as a deprecated alias (prints a one-line
> warning). `BOARD` is preferred because `MACHINE` collides with bitbake's
> own `MACHINE` variable (the yocto machine name, e.g. `raspberrypi5-wendyos`).

#### Option B: Manual Steps

1. **Clone the repository** (or place it in your working directory):
   ```bash
   cd /path/to/project
   git clone git@github.com:wendylabsinc/meta-wendyos-jetson.git meta-wendyos
   cd meta-wendyos
   git checkout <branch>
   ```

2. **Run the bootstrap script**:

   Switch back to working folder and run the `bootstrap` script, setting
   the `BOARD` environment variable to the target board id:

   ```bash
   cd /path/to/project
   BOARD=<board-id> ./meta-wendyos/bootstrap.sh
   ```

   The full list of supported board ids lives in `conf/template/boards/`
   (one directory per board). Each board directory contains a self-contained
   `local.conf` and `bblayers.conf` that pull in shared fragments from
   `conf/template/include/{local,bblayers}/` via BitBake `require`. Adding a
   new board means creating one directory with those two files — no
   `bootstrap.sh` change required. For example:

   ```bash
   BOARD=jetson-orin-nano-nvme ./meta-wendyos/bootstrap.sh
   BOARD=jetson-orin-nano-sd   ./meta-wendyos/bootstrap.sh
   BOARD=jetson-agx-orin       ./meta-wendyos/bootstrap.sh
   BOARD=rpi5-sd               ./meta-wendyos/bootstrap.sh
   BOARD=rpi5-nvme             ./meta-wendyos/bootstrap.sh
   BOARD=qemu-arm64            ./meta-wendyos/bootstrap.sh
   ```

   `MACHINE=<board-id>` remains supported as a deprecated alias (prints a
   warning). Prefer `BOARD=` — it avoids collision with bitbake's `MACHINE`
   (the yocto machine name like `raspberrypi5-wendyos`, a different concept).

   The bootstrap script will:
   - Validate that the meta-layer is within the working directory
   - Clone all required Yocto layers (`poky`, `meta-openembedded`, `meta-tegra`, etc.)
   - Create the `build` directory using the meta layer `conf/template` configuration templates
   - Set up the Docker build environment in `docker`
   - Build the Docker image (only if it does not already exist)

3. **Customize build configuration** (optional):

   Edit `build/conf/local.conf` to customize:
   - `DL_DIR` - Download directory for source tarballs (recommended for caching)
   - `SSTATE_DIR` - Shared state cache directory (speeds up rebuilds)
   - `MACHINE` - Yocto machine name. This is written to `build/conf/local.conf`
     by `bootstrap.sh` based on the board id you passed in. The board id must
     match a directory `conf/template/boards/<board-id>/`; that directory's
     `local.conf` sets the Yocto `MACHINE` variable. Current mapping:
     - `jetson-orin-nano-nvme`   → `jetson-orin-nano-devkit-nvme-wendyos`
     - `jetson-orin-nano-sd`     → `jetson-orin-nano-devkit-wendyos`
     - `jetson-agx-orin`         → `jetson-agx-orin-devkit-nvme-wendyos`
     - `rpi5-sd`                 → `raspberrypi5-wendyos`
     - `rpi5-nvme`               → `raspberrypi5-nvme-wendyos`
     - `qemu-arm64`              → `qemuarm64-wendyos`
   - `WENDYOS_FLASH_IMAGE_SIZE` - Flash image size: "64GB"):
     - `"4GB"` - 3.2GB Mender storage (~1.3GB per rootfs partition)
     - `"8GB"` - 6.4GB Mender storage (~2.9GB per rootfs partition)
     - `"16GB"` - 12.8GB Mender storage (~6GB per rootfs partition)
     - `"32GB"` - 25.7GB Mender storage (~12GB per rootfs partition)
     - `"64GB"` - 51GB Mender storage (~25GB per rootfs partition) [**default**]

4. **Build the image**

   Follow instructions displayed by the `bootstrap.sh`:

   ```bash
   # start the Docker container
   cd ./docker
   ./docker-util.sh run

   # build the Linux image inside the container
   cd ./wendyos
   . ./repos/poky/oe-init-build-env build
   bitbake wendyos-image
   ```

   Depending on the hardware configuration, the build process can take several hours on the first run (when the `download` and `sstate-cache` folders are empty!).

### Flash the SD Card or NVMe

The build produces a flash package at:
```
build/tmp/deploy/images/<machine>/wendyos-image-<machine>.rootfs.tegraflash.tar.gz
```

**Important**: The flashing script differs based on your target machine:
- **NVMe** (`jetson-orin-nano-devkit-nvme-wendyos`, `jetson-agx-orin-devkit-nvme-wendyos`) → use `doexternal.sh`
- **SD card** (`jetson-orin-nano-devkit-wendyos`) → use `dosdcard.sh`

#### For SD Card Builds

**Option 1: Directly Flash to SD Card**

```bash
cd /path/to/project
mkdir ./deploy
tar -xzf ./build/tmp/deploy/images/jetson-orin-nano-devkit-wendyos/wendyos-image-*.tegraflash.tar.gz -C ./deploy
cd ./deploy
sudo ./dosdcard.sh /dev/sdX
```

Replace `/dev/sdX` with the actual SD card device (e.g., `/dev/sdb`).

**Warning**: This will erase all data on the device!

**Option 2: Create a Flashable .img File**

```bash
cd /path/to/project
mkdir ./deploy
tar -xzf ./build/tmp/deploy/images/jetson-orin-nano-devkit-wendyos/wendyos-image-*.tegraflash.tar.gz -C ./deploy
cd ./deploy
sudo ./dosdcard.sh wendyos.img
```

This creates `wendyos.img`, which you can flash using dd or GUI tools (see below).

#### For NVMe Builds

**Option 1: Directly Flash to NVMe**

```bash
cd /path/to/project
mkdir ./deploy
tar -xzf ./build/tmp/deploy/images/jetson-orin-nano-devkit-nvme-wendyos/wendyos-image-*.tegraflash.tar.gz -C ./deploy
cd ./deploy
sudo ./doexternal.sh /dev/nvme0n1
```

Replace `/dev/nvme0n1` with your actual NVMe device path.

**Warning**: This will erase all data on the device!

**Option 2: Create a Flashable .img File**

```bash
cd /path/to/project
mkdir ./deploy
tar -xzf ./build/tmp/deploy/images/jetson-orin-nano-devkit-nvme-wendyos/wendyos-image-*.tegraflash.tar.gz -C ./deploy
cd ./deploy
sudo ./doexternal.sh -s 64G wendyos-nvme.img
```

**Important**: You **must** specify the size with `-s` parameter, and it **must match** your `WENDYOS_FLASH_IMAGE_SIZE` setting in `build/conf/local.conf`:
- `-s 4G` for `WENDYOS_FLASH_IMAGE_SIZE = "4GB"`
- `-s 8G` for `WENDYOS_FLASH_IMAGE_SIZE = "8GB"`
- `-s 16G` for `WENDYOS_FLASH_IMAGE_SIZE = "16GB"`
- `-s 32G` for `WENDYOS_FLASH_IMAGE_SIZE = "32GB"`
- `-s 64G` for `WENDYOS_FLASH_IMAGE_SIZE = "64GB"`

**Warning**: Using a mismatched size will result in a corrupted or non-bootable image!

This creates `wendyos-nvme.img`, which you can flash using dd or GUI tools (see below).

#### Flashing the .img File

**Command line (works for both SD card and NVMe):**
```bash
# For SD card
sudo dd if=wendyos.img of=/dev/sdX bs=4M status=progress oflag=sync conv=fsync

# For NVMe
sudo dd if=wendyos-nvme.img of=/dev/nvme0n1 bs=4M status=progress oflag=sync conv=fsync

sync
```

**GUI tools:**
- balenaEtcher (recommended)
- Raspberry Pi Imager
- GNOME Disks

### Alternative: Flashing with initrd-flash (USB Recovery Mode)

The `initrd-flash` method is an alternative USB-based flashing approach provided by NVIDIA. Use this method when:

- **Your device is bricked or won't boot** (recovery/unbrick method)
- You want to flash internal storage (NVMe/eMMC) over USB
- You need to flash a device without removing the storage
- You're setting up devices for the first time
- Standard `doexternal.sh` doesn't work for your setup
- You need NVIDIA's official recovery mode flashing

**When NOT to use initrd-flash:**
- You already have WendyOS installed (use Mender OTA updates instead)
- You're flashing external SD cards (use `dosdcard.sh` instead)
- You need to create portable .img files (use `doexternal.sh -s` or `dosdcard.sh` instead)

#### Prerequisites

- NVIDIA Jetson Orin Nano Developer Kit
- USB-C cable (for recovery mode connection)
- Host PC running Linux (Ubuntu 20.04+ recommended), MacOS
- Device in recovery mode

#### Recovery from Bricked Device

If your device won't boot (corrupted bootloader, failed update, etc.), the `initrd-flash` method is your **recovery tool**. Recovery mode bypasses the internal storage and boots a minimal system from USB, allowing you to reflash the device completely.

**Signs your device is bricked:**
- Device powers on but shows no output (no UART, no display, no network)
- Bootloader corruption from failed update
- Partition table corruption
- Repeated boot loops
- Device won't respond to any boot attempts

In these cases, `initrd-flash` is often the **only way** to recover the device without replacing hardware.

#### Steps to Flash with initrd-flash

**1. Unpack the Flash Package**

```bash
cd /path/to/project
mkdir -p ./deploy
cd ./deploy

# Extract the tegraflash package
tar -xzf ../build/tmp/deploy/images/jetson-orin-nano-devkit-nvme-wendyos/wendyos-image-jetson-orin-nano-devkit-nvme-wendyos.tegraflash.tar.gz

# Verify the initrd-flash script exists
ls -la initrd-flash.sh
```

**2. Put Device in Recovery Mode**

The Jetson Orin Nano Developer Kit does **not** have a physical Force Recovery button. You must short pins on the button header:

- Power off the Jetson device completely
- Connect the USB-C port (next to the power jack) to your host PC
- Locate the button header on the carrier board (typically near the GPIO header)
  - This is a single row of pins (not a 2-column header)
  - Look for pins labeled **FC REC (Force Recovery)** [9] and **GND (Ground)** [10]
  - These pins are usually adjacent to each other on the header
- Short the FC REC and GND pins using a jumper wire or tweezers
  - You need a connection between Force Recovery and Ground
- While keeping the pins shorted, press the **Power button** or plug in power
- Wait a couple of seconds, then remove the short
- The device should now be in recovery mode

**Note**: Consult your carrier board documentation or silkscreen labels to identify the exact Force Recovery and Ground pin locations.

**3. Verify Recovery Mode**

On your host PC, verify the device is detected:

```bash
lsusb | grep -i nvidia
# Should show: "NVIDIA Corp. APX"
```

If not detected:
- Try a different USB cable (must support data transfer)
- Try a different USB port on your PC
- Verify you shorted the correct pins (FC REC and GND)
- Ensure the short was maintained during power-on
- Check the carrier board silkscreen or documentation for pin labels
- Try shorting the pins again and power cycling
- Check that your user is in the `dialout` group: `sudo usermod -aG dialout $USER`

**Tip**: The button header pins are typically labeled on the carrier board silkscreen. Look for "FC REC" or "RECOVERY" and "GND" markings next to the pins.

**4. Disable Desktop Automounting**

The initrd-flash process exposes the Jetson's storage as a USB mass storage device on the host. Desktop environments (GNOME, KDE, etc.) will automatically mount these partitions as they are created, which causes the flash script to fail with `ERR: unmount` / `udisks-error-quark` errors.

Disable automounting before flashing:

```bash
# GNOME
gsettings set org.gnome.desktop.media-handling automount false

# KDE (Plasma 5+)
qdbus org.freedesktop.UDisks2 /org/freedesktop/UDisks2/Manager org.freedesktop.DBus.Properties.Set org.freedesktop.UDisks2.Manager AutomaticMountingEnabled false
```

Re-enable after flashing:

```bash
# GNOME
gsettings set org.gnome.desktop.media-handling automount true
```

**5. Run the initrd-flash Script**

```bash
cd /path/to/project/deploy

# Run the flash script (no arguments needed - config is in .env.initrd-flash)
sudo ./initrd-flash.sh

# Optional: Skip bootloader flashing (rootfs only)
# sudo ./initrd-flash.sh --skip-bootloader

# Optional: Erase NVMe before flashing
# sudo ./initrd-flash.sh --erase-nvme
```

**Note:** The script reads configuration from `.env.initrd-flash` (created during build), which contains:
- Machine type (e.g., `jetson-orin-nano-devkit-nvme-wendyos`, `jetson-agx-orin-devkit-nvme-wendyos`, `jetson-orin-nano-devkit-wendyos`)
- Target device (NVMe or eMMC)
- Board IDs and other hardware parameters

No command-line arguments are needed for machine/device - it's all pre-configured!

Available Options:
- `--skip-bootloader` - Skip boot partition programming (rootfs only)
- `--erase-nvme` - Erase NVMe drive during flashing
- `--usb-instance <instance>` - Specify USB instance (for multiple devices)
- `-u <keyfile>` - PKC key file for signing
- `-v <keyfile>` - SBK key file for signing
- `-h` or `--help` - Display usage information

**What Gets Flashed:**

The `initrd-flash` script performs a complete system flash including all firmware and partitions.

Firmware Components:
- **UEFI Firmware** - `uefi_jetson.bin`, `uefi_jetson_minimal.bin`
- **Boot Chain** - MB1 (`mb1_t234_prod.bin`), MB2 (`mb2_t234.bin`)
- **PSC Firmware** - PSC BL1 (`psc_bl1_t234_prod.bin`), PSC FW (`pscfw_t234_prod.bin`)
- **Additional Firmware** - 20+ components including SPE, MCE, BPMP, DCE, XUSB, etc.
- **Trusted OS** - `tos-optee_t234.img`

Storage Components:
- **ESP (EFI System Partition)** - Contains UEFI boot files (`esp.img`)
- **Kernel** and **Device Tree Blobs**
- **Rootfs Partitions** - APP_a and APP_b (A/B redundancy for Mender)
- **Partition Table** - GPT layout defined in flash XML

Bootloader Location:
- SPI Flash (device 3:0) **OR** eMMC boot partitions (device 0:3) - device-dependent
- Rootfs written to NVMe (device 9:0) or eMMC user partition (device 1:3)

Why This Matters:
- **Fixes bootloader corruption** - Reflashes complete boot chain (MB1, MB2, PSC, UEFI)
- **Updates bootloader versions** - Installs all firmware from the tegraflash package
- **Recovers from failed firmware updates** - Replaces all boot components
- **Resets partition layout** - Creates fresh GPT partition table
- **Unbricks devices** - Works even when storage is completely corrupted

Important Notes:
- The script will upload a recovery kernel and initramfs to the device
- The device will boot into the recovery system
- Flashing will proceed automatically (takes ~5-15 minutes)
- Do NOT disconnect USB or power during this process
- **All data on the device will be erased** (bootloader, rootfs, data partition)

**6. Monitor the Flash Process**

The script will display progress:
```
*** Flashing target device started. ***
Waiting for device to expose ssh ...
SSH ready
Flashing to mmcblk0p1 ...
Writing bootloader ...
Writing kernel ...
Writing rootfs ...
*** The target device has been flashed successfully. ***
*** Reboot the target device ***
```

**6. Reboot the Device**

After successful flashing:
```bash
# The device will automatically reboot, or you can manually power cycle it
# Remove the USB cable
# The device should boot into WendyOS
```

**7. Verify Boot**

Connect via SSH (over USB or Ethernet):
```bash
# Find device IP (check DHCP, use .local name, or USB network)
ssh wendy@wendy-<adjective>-<noun>.local
# Default password: wendy

# Verify system info
cat /etc/os-release
uname -a
```

### Available Images

The build produces multiple image formats:
- `tegraflash` - Complete Tegra flash package (bootloader, kernel, rootfs, DTBs)
- `mender` - Mender OTA update artifact (.mender file)
- `dataimg` - Data partition image
- `ext4` - Raw rootfs (for debugging)

## USB Gadget Networking

When a Jetson running WendyOS is connected via USB-C, it exposes a composite USB gadget
(NCM network + ACM serial). The Jetson configures `usb0` as a **DHCP client** — it does
not assign its own address. The host must provide an IP via DHCP.

### Linux host

Use `scripts/manage-net-sharing.sh` from this repository:

```bash
# List detected WendyOS gadget devices:
./scripts/manage-net-sharing.sh list

# Auto-detect interface and enable internet sharing:
./scripts/manage-net-sharing.sh enable

# Check status (shows host IP and board IP once connected):
./scripts/manage-net-sharing.sh status

# Test connectivity:
./scripts/manage-net-sharing.sh test

# Disable sharing:
./scripts/manage-net-sharing.sh disable
```

The script auto-detects the Jetson by USB manufacturer/product string or USB ID
(`1d6b:0104`). It uses NetworkManager `method=shared`, which assigns `10.42.0.1` to the
host, starts dnsmasq for DHCP, and enables NAT so the Jetson can reach the internet
through the host.

### macOS host

Enable **Internet Sharing** in **System Settings → General → Sharing → Internet Sharing**:
- Share connection from: **Wi-Fi** (or whichever interface has internet)
- To computers using: the Jetson's USB NCM interface (shown as "RNDIS/Ethernet Gadget"
  or "Ethernet Adapter" depending on macOS version)

macOS assigns itself `192.168.2.1` and hands the Jetson an address in `192.168.2.x`.

> **Note:** QEMU networking (`10.43.0.0/24`) is independent of Jetson USB gadget
> networking (`10.42.0.0/24`). Both can be active simultaneously without conflict.

For a detailed explanation of the full USB-C enumeration stack, see
[`docs/usb-gadget-vbus-notification-deep-dive.md`](docs/usb-gadget-vbus-notification-deep-dive.md).

## Mender OTA Updates

The system includes Mender for Over-The-Air updates with A/B partition redundancy.

### Partition Layout

**SD Card (mmcblk0):**
- `/dev/mmcblk0p1` - Root filesystem A
- `/dev/mmcblk0p2` - Root filesystem B
- `/dev/mmcblk0p11` - Boot partition (shared)
- `/dev/mmcblk0p15` - Data partition (persistent)

**NVMe:**
- `/dev/nvme0n1p1` - Root filesystem A
- `/dev/nvme0n1p2` - Root filesystem B
- `/dev/nvme0n1p11` - Boot partition (shared)
- `/dev/nvme0n1p15` - UDA partition (NVIDIA reserved, not used by wendyos)
- `/dev/nvme0n1p17` - Mender data partition (expandable, mounted at `/data`)

### Manual Update

For testing or offline updates, you can manually install a `.mender` artifact without a Mender server:

**1. Transfer the artifact to the device:**

```bash
scp wendyos-image-*.mender root@<device-ip>:/tmp/
```

**2. Install the update:**

```bash
ssh root@<device-ip>
sudo mender-update install /tmp/wendyos-image-*.mender
```

**3. Reboot to apply:**

```bash
sudo reboot
```

**4. Verify the update:**

After reboot, check the new version:

```bash
cat /etc/os-release | grep VERSION_ID
mender-update show-artifact
```

**5. Commit the update:**

If the system boots successfully and you're satisfied with the new version:

```bash
sudo mender-update commit
```

**Note:** If you don't commit, Mender will automatically roll back to the previous version on the next reboot.

### Mender Server Update

For production deployments, use the Mender server for centralized OTA update management.

#### Setting Up Mender Server

#### 1. Install Dependencies

```bash
sudo apt install docker.io docker-compose-plugin git
sudo systemctl enable --now docker
```

#### 2. Install Mender Demo Server

```bash
cd <server_dir>
git clone https://github.com/mendersoftware/mender-server
cd mender-server
git checkout v4.0.1
```

#### 3. Configure DNS Resolution

On both the server and all Jetson devices, add the server IP to `/etc/hosts`:

```bash
echo '<server_ip> docker.mender.io s3.docker.mender.io' | sudo tee -a /etc/hosts
```

**Note**: Port `443/tcp` must be open on the server.

#### 4. Start Mender Server

```bash
docker compose up -d

# Create admin user (first run only)
docker compose exec useradm useradm create-user \
  --username "admin@docker.mender.io" \
  --password "password123"
```

#### 5. Verify Server Status

```bash
docker compose ps
docker compose logs -f api-gateway deployments deviceauth
```

#### Device Configuration

The Mender client on the Jetson device is pre-configured to connect to `https://docker.mender.io`. Ensure the `/etc/hosts` entry is set (see step 3 above).

The server's TLS certificate is already included in the image at `/etc/mender/server.crt`.

#### Deploy an Update

1. Open https://docker.mender.io/ in your browser
2. Log in with `admin@docker.mender.io` / `password123`
3. Go to **Devices → Pending** and accept your Jetson device
4. Upload a `.mender` artifact under **Artifacts**
5. Create a deployment under **Deployments → Create deployment**
6. Monitor the update progress on the device

#### Mender Configuration

- **Server URL**: `https://docker.mender.io`
- **Update poll interval**: 30 minutes
- **Inventory poll interval**: 8 hours
- **Artifact naming**: `${IMAGE_BASENAME}-${MACHINE}-${IMAGE_VERSION_SUFFIX}`

#### Tear Down Server

```bash
# Stop and remove containers + volumes (wipes all data)
docker compose down -v

# Optional: Remove server files
cd <server_dir>/..
rm -rf mender-server
```

## Advanced Configuration

### Custom Variables in bootstrap.sh

You can modify these variables in `bootstrap.sh` before running:
- `IMAGE_NAME` - Base name for the OS (default: "wendyos")
- `USER_NAME` - Docker container username (default: "dev")
- `YOCTO_BRANCH` - Yocto release branch (default: "scarthgap")

### Build Configuration Variables

In `build/conf/local.conf`:
- `WENDYOS_FLASH_IMAGE_SIZE` - Flash image size: "4GB", "8GB", "16GB", "32GB", "64GB" (default: "64GB" — set per-board in `conf/template/boards/<id>/local.conf`; Tegra only)
- `WENDYOS_DEBUG` - Enable debug packages (default: 0)
- `WENDYOS_DEBUG_UART` - Enable UART debug output (default: 0)
- `WENDYOS_USB_GADGET` - Enable USB gadget mode (default: 0)
- `WENDYOS_PERSIST_JOURNAL_LOGS` - Persist logs to storage (default: 0)

**Note**: Choose `WENDYOS_FLASH_IMAGE_SIZE` based on your target storage device capacity and expected rootfs size. Larger images provide more space for root filesystems and future updates.

### Runtime Identity

Runtime consumers (e.g. the wendy agent) read two files from `/etc/wendyos/`:

- **`/etc/wendyos/device-type`** — shell-sourceable, board + yocto machine.
  Example for Jetson Orin Nano (NVMe):

  ```
  BOARD=jetson-orin-nano-nvme
  MACHINE=jetson-orin-nano-devkit-nvme-wendyos
  ```

  `BOARD` is the WendyOS board id (the value you pass to `bootstrap.sh` as
  `BOARD=`), set by `WENDYOS_BOARD_ID` in `conf/machine/<machine>.conf`.
  `MACHINE` is bitbake's full yocto machine name.

- **`/etc/wendyos/version.txt`** — the installed OS version, e.g.
  `WendyOS-0.14.0`. Reflects the currently running rootfs (always fresh after
  an OTA update).

Runtime consumers can `. /etc/wendyos/device-type` and branch on `$BOARD`
without maintaining their own board-to-machine lookup table.

#### Where these files live on disk

The `/etc/wendyos/` directory is bind-mounted from `/data/etc/wendyos/` on
Tegra (via `setup-etc-binds.sh`), so runtime-generated identity (`device-uuid`,
`device-name`) persists across Mender OTA updates. The two build-time files
above have different refresh semantics and are seeded differently:

| File | Installed by recipe to | Runtime lifecycle |
|---|---|---|
| `device-type` | `/etc/wendyos/device-type` (rootfs) | `setup-etc-binds.sh` seeds to `/data` on first boot only — hardware identity, never changes |
| `version.txt` | `/usr/lib/wendyos/version.txt` (authoritative) + `/etc/wendyos/version.txt` symlink | `setup-etc-binds.sh` overwrites `/etc/wendyos/version.txt` from `/usr/lib/` on every boot — stays current across OTA |

On RPi and QEMU (no `/data`), `setup-etc-binds.sh` doesn't run. `device-type`
lives directly on rootfs and `version.txt` is a symlink to the `/usr/lib/`
copy — both always current.

### Per-Board Repo Overrides

The default upstream layer pinning (commit hashes for `poky`, `meta-tegra`,
`meta-raspberrypi`, etc.) lives in `bootstrap.sh` as `SRCREV_*` variables.
A single default is shared by every board and is fine for today's targets —
all machines build against the same layer commits.

Each board directory contains an optional `repos.overrides` file
(`conf/template/boards/<board-id>/repos.overrides`). When present, it is
`source`d by `bootstrap.sh` after the defaults are set and before the repos
list is built, letting a board override one or more layers without touching
the shared defaults or the other boards.

Three override shapes are supported:

- **Pin a different commit** — uncomment and edit the relevant line in the
  placeholder:
  ```sh
  SRCREV_TEGRA="<commit-hash>"
  ```
- **Replace a source URL** (e.g. to use a fork):
  ```sh
  URL_TEGRA="https://github.com/my-org/meta-tegra-fork.git"
  ```
- **Add an extra clone** that coexists with the defaults — useful when a
  board needs a parallel copy of a layer at a different branch:
  ```sh
  SRCREV_TEGRA_THOR="<commit-hash>"
  REPOS_EXTRA+=(
      "1|https://github.com/OE4T/meta-tegra.git|meta-tegra-thor|${SRCREV_TEGRA_THOR}"
  )
  ```
  The board's `bblayers.conf` then points at `${TOPDIR}/../repos/meta-tegra-thor`
  (via an appropriate include fragment) instead of the default `repos/meta-tegra`.

A `repos.overrides` file with every line commented out is equivalent to no
overrides — today's shipped placeholders are exactly that. The shared
`repos/` directory holds at most one clone per folder name, so two boards
that override the same folder to different commits will cause a re-checkout
when switching. Use `REPOS_EXTRA` with a different folder name to avoid that.

## Raspberry Pi 5

WendyOS supports the **Raspberry Pi 5** as an alternative target. The RPi5 build uses
[meta-raspberrypi](https://git.yoctoproject.org/meta-raspberrypi) as its BSP layer and
produces a `.wic` disk image (SD card or NVMe). Mender OTA is not supported on RPi5.

### Supported Machines

| Machine | Boot device | WKS file |
|---------|-------------|----------|
| `raspberrypi5-wendyos` | SD card (default) | `rpi-partuuid.wks` |
| `raspberrypi5-nvme-wendyos` | NVMe via passive PCIe adapter | `rpi-nvme-partuuid.wks` |

Both machines include Wi-Fi, Bluetooth, and USB gadget (NCM) support. UART console is
enabled on `ttyAMA0` at 115200 baud.

### Build

1. **Bootstrap** the build environment for RPi5:

   ```bash
   cd /path/to/project
   # SD card boot (yocto MACHINE = raspberrypi5-wendyos)
   BOARD=rpi5-sd ./meta-wendyos/bootstrap.sh
   # or NVMe boot (yocto MACHINE = raspberrypi5-nvme-wendyos)
   BOARD=rpi5-nvme ./meta-wendyos/bootstrap.sh
   ```

   The bootstrap script copies `build/conf/bblayers.conf` and
   `build/conf/local.conf` from the per-board directory
   `conf/template/boards/<board-id>/`. Those files `require` shared fragments
   from `conf/template/include/{local,bblayers}/`. Choose the right board id
   up front — there is no in-tree switch after bootstrap.

2. **Build the image** inside the Docker container:

   ```bash
   cd ./docker
   ./docker-util.sh run

   # Inside the container:
   cd ./wendyos
   . ./repos/poky/oe-init-build-env build
   bitbake wendyos-image
   ```

   The build produces:
   ```
   build/tmp/deploy/images/<machine>/wendyos-image-<machine>.rootfs.wic
   build/tmp/deploy/images/<machine>/wendyos-image-<machine>.rootfs.wic.bmap
   ```

### Flash the Image

Use `bmaptool` (faster, recommended) or `dd` to write the `.wic` image to the target
storage device.

**With bmaptool:**
```bash
sudo bmaptool copy wendyos-image-<machine>.rootfs.wic /dev/sdX
```

**With dd:**
```bash
sudo dd if=wendyos-image-<machine>.rootfs.wic of=/dev/sdX bs=4M status=progress conv=fsync
sync
```

Replace `/dev/sdX` with the actual device (e.g., `/dev/sdb` for SD card, `/dev/nvme0n1`
for NVMe).

**Warning**: This will erase all data on the target device!

For SD card builds, insert the flashed card into the RPi5 and power on. For NVMe builds,
ensure the NVMe drive is connected via a PCIe adapter and that the EEPROM boot order is
configured to boot from NVMe (see `rpi-eeprom-nvme-config` package included in the NVMe
machine).

## QEMU (ARM64)

QEMU provides a virtual ARM64 machine for development and testing without physical hardware.
It runs the same WendyOS image as physical devices but uses `virtio-net` instead of the USB
gadget for networking.

### QEMU Prerequisites

Install `qemu-system-aarch64` on your host:

```bash
# Debian/Ubuntu
sudo apt install qemu-system-arm

# Fedora/RHEL
sudo dnf install qemu-system-aarch64

# Arch
sudo pacman -S qemu-system-aarch64
```

### QEMU Build

```bash
make setup BOARD=qemu-arm64
make build
```

The build produces:
```
build/tmp/deploy/images/qemuarm64-wendyos/wendyos-image-qemuarm64-wendyos.rootfs.ext4
build/tmp/deploy/images/qemuarm64-wendyos/Image
```

### Run

Run the QEMU image directly from the **host** (not inside the Docker container):

```bash
./scripts/run-qemu.sh
```

**Options:**
```bash
./scripts/run-qemu.sh --build-dir /path/to/build   # custom build directory
./scripts/run-qemu.sh --usb 1234:5678               # pass through a USB device
./scripts/run-qemu.sh --dry-run                     # show what would run without executing
```

To exit QEMU: press **Ctrl-A**, then **X**.

### Networking

`run-qemu.sh` automatically sets up host networking on first run by calling
`scripts/manage-qemu-network-host.sh setup`. This creates:

- A TAP interface `tap-wendyos` and bridge `br-wendyos` on the host
- Host IP `10.43.0.1/24`, QEMU guest receives an address in `10.43.0.10–10.43.0.250` via dnsmasq
- NAT via iptables for internet access from inside the VM

You may be prompted for your `sudo` password since creating network interfaces requires
elevated privileges.

> **Note:** QEMU networking (`10.43.0.0/24`) is independent of Jetson USB gadget networking
> (`10.42.0.0/24`). Both can be active simultaneously on the same host without conflict.
> Use `scripts/manage-net-sharing.sh` to manage internet sharing for a connected Jetson device.

### Cleanup

The bridge and TAP interface persist after QEMU exits (so subsequent runs start faster).
When you no longer need the QEMU network, remove it:

```bash
sudo ./scripts/manage-qemu-network-host.sh cleanup
```

To check the current state:

```bash
./scripts/manage-qemu-network-host.sh status
```

## Architecture Notes

- **Yocto Version**: `Scarthgap`
- **Base Layer**: `meta-tegra` (NVIDIA Jetson BSP)
- **Init System**: `systemd`
- **Package Manager**: `RPM`
- **Boot Method**: UEFI with extlinux
- **OTA System**: Mender v5.0.x
- **Display Features**: Removed (headless embedded system)

## Building on macOS

### Overview

Building WendyOS on macOS is fully supported through Docker Desktop. The build process runs inside an Ubuntu 24.04 LTS container, making it identical to building on a Linux host.

### macOS-specific Considerations

1. **Docker Desktop Resources**: Yocto builds are resource-intensive. Configure Docker Desktop with:
   - At least 8GB RAM (16GB recommended)
   - 4+ CPUs
   - 150GB+ disk space

2. **Build Performance**: Builds on macOS may be slower than native Linux due to:
   - Docker's virtualization layer
   - File system performance differences (VirtioFS is recommended in Docker Desktop settings)

3. **Network Differences**: On macOS, `--network=host` doesn't work as it does on Linux. The build scripts automatically handle this by using Docker's default bridge networking, which is sufficient for the build process.

4. **X11 Support**: X11 forwarding (for GUI tools like `devtool`) is not available by default on macOS. If needed, install XQuartz and configure it manually. However, Yocto command-line builds work without X11.

### Flashing

Use the interactive flash tool (works on both macOS and Linux):

```bash
make flash-to-external
```

This will:
1. Create a flashable `.img` file (if not already created)
2. List available external drives
3. Prompt you to select the target disk
   - macOS: e.g., `disk42`
   - Linux: e.g., `sdb` or `nvme0n1`
4. Flash the image and safely eject the drive

**Non-interactive mode** (for scripting):
```bash
# macOS
make flash-to-external FLASH_DEVICE=/dev/disk42 FLASH_CONFIRM=yes

# Linux
make flash-to-external FLASH_DEVICE=/dev/sdb FLASH_CONFIRM=yes
```

### Troubleshooting macOS Builds

**Issue: Docker build fails with network errors**
- Ensure Docker Desktop has internet access
- Try restarting Docker Desktop

**Issue: Build runs out of disk space**
- Increase Docker Desktop disk allocation in Preferences → Resources
- Clean up old images: `docker system prune -a`
- Clear the Yocto sstate-cache if needed

**Issue: Permission denied errors on mounted volumes**
- Ensure the project directory is in a location Docker Desktop can access
- Check Docker Desktop → Preferences → Resources → File Sharing

**Issue: Build is very slow**
- Use VirtioFS in Docker Desktop settings for better file system performance
- Increase allocated CPUs and memory
- Consider using a shared `sstate-cache` and `downloads` directory across builds

## License

TBD
