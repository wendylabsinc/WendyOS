# RPi5-specific image configuration
# This file is included when building for RPi machines (MACHINEOVERRIDES contains 'rpi')

inherit partuuid-rpi

# RPi5 has no Mender; suppress the unconditional MENDER_CONNECT_ENABLE=1 set by wendyos-image.bb
MENDER_CONNECT_ENABLE = "0"

# RPi5-specific packages
IMAGE_INSTALL:append = " \
    packagegroup-wendyos-rpi \
    "

# Container runtime
IMAGE_INSTALL:append = " \
    ${@oe.utils.ifelse(d.getVar('WENDYOS_CONTAINER_RUNTIME') == '1', ' packagegroup-wendyos-container', '')} \
    "

# wendyos-image.bb appends ext4 unconditionally; RPi5 only needs wic
IMAGE_FSTYPES:remove = "ext4"

# Remove Tegra-only packages added by tegra-image.inc or wendyos-image.bb.
# Layer separation removes these recipes from the parse tree, but IMAGE_INSTALL
# is resolved at task-scheduling time — absent packages must be explicitly removed.
IMAGE_INSTALL:remove = " \
    mender-esp \
    tegra-bootcontrol-overlay \
    packagegroup-wendyos-tegra \
    packagegroup-nvidia-container \
    nvidia-container-config \
    setup-nv-boot-control \
    usb-gadget-modules \
    usb-network-tuning \
    e2fsprogs-mke2fs \
    util-linux-mount \
    "

# WIC build dependencies
do_image_wic[depends] += " \
    rpi-cmdline:do_deploy \
    wic-tools:do_populate_sysroot \
    e2fsprogs-native:do_populate_sysroot \
    gptfdisk-native:do_populate_sysroot \
"
do_image_wic[depends] += "${PN}:do_generate_partuuids"
WIC_CREATE_EXTRA_ARGS ?= "--no-fstab-update"
