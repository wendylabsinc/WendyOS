SUMMARY = "Bootloader update marker file for Mender UEFI capsule updates"
DESCRIPTION = "Installs a marker file that triggers UEFI capsule staging during \
Mender OTA updates. When this package is installed, the switch-rootfs state \
script will stage the bootloader capsule to the ESP for atomic rootfs+bootloader \
updates."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

# This is a marker-only package, no source files needed
ALLOW_EMPTY:${PN} = "1"

do_install() {
    # Create the bootloader update marker file
    # This signals switch-rootfs to stage the UEFI capsule during deployment
    install -d ${D}${localstatedir}/lib/wendyos
    touch ${D}${localstatedir}/lib/wendyos/update-bootloader
}

FILES:${PN} = "${localstatedir}/lib/wendyos/update-bootloader"

PACKAGE_ARCH = "${MACHINE_ARCH}"
