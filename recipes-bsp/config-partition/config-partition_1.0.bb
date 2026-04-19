SUMMARY = "FAT32 config partition image for Tegra flash"
DESCRIPTION = "Creates a FAT32 partition image deployed to DEPLOY_DIR_IMAGE \
for inclusion in the Tegra tegraflash package. RPi5 does not need this — \
WIC creates the partition directly from the WKS --fstype=vfat directive."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit deploy nopackages

DEPENDS = "dosfstools-native"

# Only Tegra machines need the deployed image (RPi5 uses WIC)
COMPATIBLE_MACHINE = "(tegra)"

do_compile() {
    dd if=/dev/zero of=${B}/config-partition.fat32.img \
       bs=1M count=${WENDYOS_CONFIG_PART_SIZE_MB}
    mkfs.fat -F 32 -n "config" ${B}/config-partition.fat32.img
}

do_deploy() {
    install -m 0644 ${B}/config-partition.fat32.img ${DEPLOYDIR}/config-partition.fat32.img
}

addtask deploy after do_compile before do_build
