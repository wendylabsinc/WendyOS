SUMMARY = "USB Network Performance Tuning"
DESCRIPTION = "Kernel parameter tuning for optimized USB gadget network performance"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://99-usb-network-performance.conf"

S = "${WORKDIR}"

do_install() {
    install -d ${D}${sysconfdir}/sysctl.d
    install -m 0644 ${WORKDIR}/99-usb-network-performance.conf ${D}${sysconfdir}/sysctl.d/
}

FILES:${PN} = "${sysconfdir}/sysctl.d/99-usb-network-performance.conf"

# Apply settings at boot via systemd-sysctl
RDEPENDS:${PN} = "systemd"
