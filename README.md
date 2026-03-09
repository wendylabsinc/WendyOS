# WendyOS for NVIDIA Jetson Developer Kits

This repository provides the meta-layer and build flow to build **WendyOS** for **NVIDIA Jetson Developer Kits**, including:
- **Jetson Orin Nano Developer Kit** (8GB)
- **Jetson AGX Orin Developer Kit** (64GB)

### Supported Hardware

| Hardware | SoC | RAM | Machine Config | Boot Device |
|----------|-----|-----|----------------|-------------|
| Jetson Orin Nano DevKit | Tegra234 | 8GB | `jetson-orin-nano-devkit-wendyos` | eMMC/SD |
| Jetson Orin Nano DevKit | Tegra234 | 8GB | `jetson-orin-nano-devkit-nvme-wendyos` | NVMe |
| Jetson AGX Orin DevKit | Tegra234 | 64GB | `jetson-agx-orin-devkit-nvme-wendyos` | NVMe |

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
    - [For eMMC/SD Card Builds](#for-emmcsd-card-builds)
    - [For NVMe Builds](#for-nvme-builds)
    - [Flashing the .img File](#flashing-the-img-file)
    - [Alternative: Flashing with initrd-flash (USB Recovery Mode)](#alternative-flashing-with-initrd-flash-usb-recovery-mode)
  - [Available Images](#available-images)
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
- [Raspberry Pi 5](#raspberry-pi-5)
  - [Supported Machines](#supported-machines)
  - [Build](#build)
  - [Flash the Image](#flash-the-image)
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
# Build for Orin Nano NVMe (default)
make build

# Build for AGX Orin 64GB (NVMe)
make build MACHINE=jetson-agx-orin-devkit-nvme-wendyos

# Build for Orin Nano SD card/eMMC
make build MACHINE=jetson-orin-nano-devkit-wendyos
```

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
   the `MACHINE` environment variable to the target machine:

   ```bash
   cd /path/to/project
   MACHINE=<machine> ./meta-wendyos/bootstrap.sh
   ```

   Supported machines: `jetson`, `rpi5`, `qemu`. For example:

   ```bash
   MACHINE=jetson ./meta-wendyos/bootstrap.sh
   MACHINE=rpi5   ./meta-wendyos/bootstrap.sh
   ```

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
   - `MACHINE` - Target machine configuration:
     - `jetson-orin-nano-devkit-nvme-wendyos` (Orin Nano - NVMe boot) [**default**]
     - `jetson-orin-nano-devkit-wendyos` (Orin Nano - eMMC/SD card boot)
     - `jetson-agx-orin-devkit-nvme-wendyos` (AGX Orin 64GB - NVMe boot)
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
- **eMMC/SD card** (`jetson-orin-nano-devkit-wendyos`) → use `dosdcard.sh`

#### For eMMC/SD Card Builds

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

**4. Run the initrd-flash Script**

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

**5. Monitor the Flash Process**

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

## Mender OTA Updates

The system includes Mender for Over-The-Air updates with A/B partition redundancy.

### Partition Layout

**eMMC/SD Card:**
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
- `WENDYOS_FLASH_IMAGE_SIZE` - Flash image size: "4GB", "8GB", "16GB", "32GB", "64GB" (default: "8GB")
- `WENDYOS_DEBUG` - Enable debug packages (default: 0)
- `WENDYOS_DEBUG_UART` - Enable UART debug output (default: 0)
- `WENDYOS_USB_GADGET` - Enable USB gadget mode (default: 0)
- `WENDYOS_PERSIST_JOURNAL_LOGS` - Persist logs to storage (default: 0)

**Note**: Choose `WENDYOS_FLASH_IMAGE_SIZE` based on your target storage device capacity and expected rootfs size. Larger images provide more space for root filesystems and future updates.

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
   MACHINE=rpi5 ./meta-wendyos/bootstrap.sh
   ```

   This copies `conf/template/bblayers.conf.rpi5` and `conf/template/local.conf.rpi5`
   into `build/conf/`. The default BitBake machine is `raspberrypi5-wendyos` (SD card).
   To target NVMe, edit `build/conf/local.conf` and change:

   ```
   MACHINE = "raspberrypi5-nvme-wendyos"
   ```

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
