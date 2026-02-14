
DESCRIPTION = "USB gadget setup (NCM)"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = " \
    file://gadget-setup.sh \
    file://gadget-setup.service \
    "

UNPACKDIR = "${UNPACKDIR}/sources"
S = "${UNPACKDIR}"

inherit systemd

do_install() {
    install -d ${D}${bindir}
    install -m 0755 ${UNPACKDIR}/gadget-setup.sh ${D}${bindir}/
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/gadget-setup.service ${D}${systemd_system_unitdir}/
}

SYSTEMD_SERVICE:${PN} = "gadget-setup.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

RDEPENDS:${PN} += "bash"
