# QEMU-specific image configuration
# This file is included when building for QEMU machines (MACHINEOVERRIDES contains 'qemuall')

# Disable Mender features for QEMU
# QEMU uses simple single-partition layout without Mender A/B partitions
# IMPORTANT: Must be unset (not empty string) to prevent mender-maybe-setup from inheriting mender-setup
unset MENDER_FEATURES_ENABLE
MENDER_CONNECT_ENABLE = "0"

# QEMU rootfs size configuration (in KB)
# Override in local.conf: QEMU_ROOTFS_SIZE = "1048576"  # 1GB
# Override in local.conf: QEMU_ROOTFS_SIZE = "2097152"  # 2GB
# Override in local.conf: QEMU_ROOTFS_SIZE = "4194304"  # 4GB
QEMU_ROOTFS_SIZE ??= "4194304"
IMAGE_ROOTFS_SIZE:qemuall = "${QEMU_ROOTFS_SIZE}"
IMAGE_ROOTFS_EXTRA_SPACE:qemuall = "0"

# Remove packages not needed for QEMU
# Note: mender-configure, mender-connect, and python3-pip-jetson-config are now
# conditionally added in wendyos-image.bb, so no need to remove them here
# USB gadget packages are conditionally added via WENDYOS_USB_GADGET variable
# Note: mender-esp is only in tegra-image.inc (mutually exclusive), no need to remove
IMAGE_INSTALL:remove = " \
    gadget-setup \
    usb-gadget-modules \
    usb-network-tuning \
    "

# Add QEMU-specific packages
# - udev-qemu-usb0: Rename virtio-net to usb0 for NetworkManager compatibility
IMAGE_INSTALL:append = " \
    udev-qemu-usb0 \
    "
