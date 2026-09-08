# WendyOS Yocto Meta Layers Reference

WendyOS builds from one repo, `WendyOS-Builder`, whose sub-layers cover each
target. Every board builds `wendyos-image`; the board id selects the machine.

| Sub-layer | Target | Board id |
|-----------|--------|----------|
| `meta-tegra-extensions*` | NVIDIA Jetson (Orin, Thor) | `jetson-*` |
| `meta-rpi-extensions` | Raspberry Pi 3/4/5 | `rpi*` |
| `meta-x86-extensions` | Generic x86_64 PC | `generic-x86-64` |
| `meta-vm-extensions` | ARM64 and x86-64 VMs | `vm-arm64`, `vm-x86-64` |

## Common Structure

Shared recipes live at the repo root; each sub-layer adds only what its target
needs.

```
WendyOS-Builder/
├── conf/
│   ├── distro/wendyos.conf        # The distro, and its per-target includes
│   ├── machine/<machine>.conf     # One per board
│   └── template/boards/<board-id>/ # bblayers.conf, local.conf, repos.overrides
├── recipes-core/                  # wendyos-identity, wendyos-agent, images, ...
├── meta-<target>-extensions/      # Per-target recipes, bbappends and wic files
└── bootstrap.sh                   # Clones the upstream layer tree
```

## Quick Start (Any Board)

```bash
make setup BOARD=<board-id>
make build MACHINE=<machine>
```

## Layer-Specific Details

### Jetson (`meta-tegra-extensions*`)

- **Board ids**: `jetson-orin-nano-sd`, `jetson-orin-nano-nvme`, `jetson-agx-orin`,
  `jetson-agx-orin-emmc`, `jetson-agx-thor`
- **Features**: NVIDIA Container Toolkit, CUDA/TensorRT, USB gadget, A/B OTA
- **Dependencies**: meta-tegra, meta-tegra-community, meta-security

### Virtual (`meta-vm-extensions`)

Lives in WendyOS-Builder, not a separate repo.

- **Machines**: `vm-arm64-wendyos` (board id `vm-arm64`) and
  `vm-x86-64-wendyos` (`vm-x86-64`)
- **Image**: `wendyos-image`
- **Output**: `.wic` (UEFI: ESP + GRUB + A/B rootfs slots + growable `/data`)
- **Features**: virtio only, plus the A/B OTA stack via the grubenv connector.
  No USB gadget, no camera/audio/Bluetooth/GPU

```bash
make setup BOARD=vm-arm64
make build MACHINE=vm-arm64-wendyos
```

Run the result with `wendy vm` — see the *WendyOS in a Virtual Machine*
installation guide.

### Raspberry Pi (`meta-rpi-extensions`)

- **Board ids**: `rpi3-sd`, `rpi4-sd`, `rpi5-sd`, `rpi5-nvme`
- **Features**: I2C, SPI, serial console, USB gadget, A/B OTA
- **Output**: `.sdimg` / `.wic` plus `.wic.bmap`

## Common Recipes

### wendyos-identity

Generates unique device identity on first boot:
- UUID: `/etc/wendyos/device-uuid`
- Name: `/etc/wendyos/device-name` (e.g., "brave-falcon")
- Sets hostname to device name
- Registers with Avahi mDNS

### wendyos-agent

Installs wendy-agent from its GitHub release tarball:
- Binary: `/opt/wendyos/bin/wendy-agent`, symlinked from `/usr/local/bin/wendy-agent`
- Auto-updater via systemd timer
- Data: `/var/lib/wendy-agent`

### systemd-mount-containerd

Bind mounts `/data/containerd` to `/var/lib/containerd` for persistent container storage.

## Partition Layouts

### Jetson (NVMe)
| Partition | Purpose |
|-----------|---------|
| p1 | Root A (active) |
| p2 | Root B (OTA fallback) |
| p11 | Boot/EFI |
| p17 | `/data` (expandable) |

### VM/RPi
| Partition | Mount | Size |
|-----------|-------|------|
| 1 | /boot | 256MB |
| 2 | / | 4-8GB |
| 3 | /data | 2GB+ (expandable) |

## Build Environment (Docker)

All layers use Docker for macOS compatibility (case-sensitive filesystem):

```bash
# Build the Docker image
cd docker
./docker-util.sh create

# Open build shell
./docker-util.sh shell

# Inside container
source ./repos/poky/oe-init-build-env build
bitbake <image>
```

Docker volumes used:
- `wendyos-downloads` - Package downloads
- `wendyos-sstate` - Shared state cache
- `wendyos-tmp` - Build temporary files

## Configuration Variables

In `local.conf`:

```bitbake
# Enable debug features
EDGEOS_DEBUG = "1"

# Persist journal logs across reboots
EDGEOS_PERSIST_JOURNAL_LOGS = "1"

# Jetson-specific: Flash image size
EDGEOS_FLASH_IMAGE_SIZE = "64GB"

# Jetson-specific: USB gadget mode
EDGEOS_USB_GADGET = "1"
```

## Yocto Package Names

Common packages with different Yocto names:

| What you want | Package name |
|---------------|--------------|
| containerd | `containerd-opencontainers` |
| runc | `runc-opencontainers` |
| nerdctl | `nerdctl` |
| ctr | included in `containerd-opencontainers` |
| i2c-tools | `i2c-tools` |

## mDNS Service

All WendyOS devices advertise via mDNS:
- Service type: `_wendyos._udp` (NOT `_tcp`)
- Discover: `dns-sd -B _wendyos._udp local.`

## Common Issues

### Case-insensitive filesystem error (macOS)

Use Docker volumes:
```bitbake
TMPDIR = "/home/dev/yocto-tmp"
DL_DIR = "/home/dev/downloads"
SSTATE_DIR = "/home/dev/sstate-cache"
```

### Kernel metadata not found (VM)

Machine config needs KMACHINE mapping:
```bitbake
KMACHINE:vm-arm64-wendyos = "genericarm64"
```

### containerd storage not persisting

Check mount status:
```bash
systemctl status var-lib-containerd.mount
mount | grep /data
```

## Yocto Branch

All layers use **Scarthgap** branch for:
- poky
- meta-openembedded
- meta-virtualization
- meta-raspberrypi (RPi only)
- meta-tegra (Jetson only)
