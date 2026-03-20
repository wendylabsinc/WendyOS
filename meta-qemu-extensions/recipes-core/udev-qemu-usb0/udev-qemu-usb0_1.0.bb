
SUMMARY = "Udev rule to rename virtio-net to usb0 in QEMU"
DESCRIPTION = "Renames QEMU's virtio-net interface to usb0 for compatibility with WendyOS NetworkManager profiles and Wendy agent"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://70-qemu-usb0.rules"

S = "${WORKDIR}"

do_install() {
    install -d ${D}${sysconfdir}/udev/rules.d
    install -m 0644 ${WORKDIR}/70-qemu-usb0.rules ${D}${sysconfdir}/udev/rules.d/
}

FILES:${PN} = "${sysconfdir}/udev/rules.d/70-qemu-usb0.rules"

# Only needed for QEMU machines, not real hardware
COMPATIBLE_MACHINE = "qemuall"
