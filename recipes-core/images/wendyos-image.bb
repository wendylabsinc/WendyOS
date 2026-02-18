
DESCRIPTION = "WendyOS Image"
LICENSE = "MIT"

inherit core-image
inherit mender-full
inherit mender-dataimg
inherit image_types_tegra

DISTRO_FEATURES:append = " systemd"
VIRTUAL-RUNTIME_init_manager = "systemd"

# Image format types for Tegra platforms
# dataimg creates the Mender data partition filesystem (mounted at /data)
IMAGE_FSTYPES = "tegraflash.tar mender ext4 dataimg"

# Release-style naming for this image:
# - IMAGE_VERSION_SUFFIX is a common pattern to carry a release tag.
# - If unset, it falls back to DISTRO_VERSION.
IMAGE_VERSION_SUFFIX ?= "${DISTRO_VERSION}"

# Keep names reproducible for releases.
# (Avoid DATETIME here unless you WANT a new artifact for every rebuild.)
MENDER_ARTIFACT_NAME = "${IMAGE_BASENAME}-${MACHINE}-${IMAGE_VERSION_SUFFIX}"
# MENDER_ARTIFACT_NAME = "${IMAGE_BASENAME}-${DISTRO_VERSION}-${DATETIME}"

MENDER_UPDATE_POLL_INTERVAL_SECONDS    = "1800"
MENDER_INVENTORY_POLL_INTERVAL_SECONDS = "28800"
MENDER_RETRY_POLL_INTERVAL_SECONDS     = "300"
MENDER_SYSTEMD_AUTO_ENABLE = "1"

MENDER_CONNECT_ENABLE = "1"

# Apply our UEFI boot-priority overlay during flash
TEGRA_BOOTCONTROL_OVERLAYS += "boot-priority.dtbo"

IMAGE_FEATURES += " \
    ssh-server-openssh \
    empty-root-password \
    allow-root-login \
    allow-empty-password \
    package-management \
    "

# Temporarily disabled for whinlatter migration - Go modules build structure issue:
#    mender-connect
IMAGE_INSTALL:append = " \
    packagegroup-wendyos-base \
    packagegroup-wendyos-kernel \
    packagegroup-wendyos-debug \
    mender-esp \
    mender-configure \
    tegra-bootcontrol-overlay \
    python3-pip-jetson-config \
    setup-nv-boot-control \
    bluez5 \
    bluez5-obex \
    pipewire \
    wireplumber \
    pipewire-pulse \
    pipewire-alsa \
    rtkit \
    audio-config \
    "

# Note: mender-tegra-capsule-update removed - capsule staging now handled
# by switch-rootfs state script for atomic rootfs+bootloader updates

# Conditional UEFI capsule package installation
# Controlled by WENDYOS_UPDATE_BOOTLOADER (defined in conf/distro/wendyos.conf)
IMAGE_INSTALL += " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_UPDATE_BOOTLOADER') == '1', \
            ' \
                tegra-uefi-capsules \
                bootloader-update \
            ', \
            '' \
        )} \
    "

# Enable USB peripheral (gadget) support
IMAGE_INSTALL += " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_USB_GADGET') == '1', \
            ' \
                gadget-setup \
                usb-gadget-modules \
                usb-network-tuning \
                e2fsprogs-mke2fs \
                util-linux-mount \
            ', \
            '' \
        )} \
    "

# Note: gadget-network-config (standalone dnsmasq) removed
# NetworkManager's connection sharing provides DHCP via dnsmasq with DBus support

# Enable DeepStream SDK support (optional - adds ~1GB to image)
# Also enables l4t-deepstream.csv which provides:
# - GPU device nodes (/dev/nvhost-*) for tegrastats and GPU monitoring
# - CUDA compilation toolchain (headers, binaries, nvvm) for Triton/JIT
# - Additional libraries (libnuma) and monitoring paths
WENDYOS_DEEPSTREAM ?= "1"
IMAGE_INSTALL += " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEEPSTREAM') == '1', \
            ' \
                deepstream-8.0 \
            ', \
            '' \
        )} \
    "

IMAGE_ROOTFS_SIZE ?= "8192"
IMAGE_ROOTFS_EXTRA_SPACE:append = "${@bb.utils.contains("DISTRO_FEATURES", "systemd", " + 4096", "", d)}"

# A space-separated list of variable names that BitBake prints in the
# “Build Configuration” banner at the start of a build.
BUILDCFG_VARS += " \
    WENDYOS_DEBUG \
    WENDYOS_DEBUG_UART \
    WENDYOS_USB_GADGET \
    WENDYOS_PERSIST_JOURNAL_LOGS \
    WENDYOS_UPDATE_BOOTLOADER \
    WENDYOS_DEEPSTREAM \
    "
