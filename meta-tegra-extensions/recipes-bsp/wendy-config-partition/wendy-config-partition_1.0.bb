SUMMARY = "WendyOS first-boot config partition (FAT32, 64 MB)"
DESCRIPTION = "Creates a 64 MB FAT32 image labeled WENDYCONFIG for use as \
the first-boot configuration partition on NVMe storage. \
Auto-mounts as /Volumes/WENDYCONFIG on macOS."

LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

DEPENDS = "dosfstools-native"

COMPATIBLE_MACHINE = "(jetson-orin-nano-devkit-nvme-wendyos|jetson-agx-orin-devkit-nvme-wendyos|jetson-orin-nano-devkit-wendyos)"

inherit deploy nopackages

do_fetch[noexec] = "1"
do_unpack[noexec] = "1"
do_patch[noexec] = "1"
do_configure[noexec] = "1"
do_compile[noexec] = "1"
do_install[noexec] = "1"

do_deploy() {
    dd if=/dev/zero of=${WORKDIR}/wendy-config.fat32.img bs=1M count=64
    mkfs.fat -F 32 -n "WENDYCONFIG" ${WORKDIR}/wendy-config.fat32.img
    install -m 0644 ${WORKDIR}/wendy-config.fat32.img ${DEPLOYDIR}/wendy-config.fat32.img
}

addtask deploy after do_compile before do_build
