SUMMARY = "Swapfile setup service"
DESCRIPTION = "Creates and enables a 4GB swap file on /data partition to prevent memory exhaustion"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = "\
    file://swapfile-setup.service \
    file://swapfile-setup.sh \
    file://data-swapfile.swap \
"

SYSTEMD_SERVICE:${PN} = "swapfile-setup.service data-swapfile.swap"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

# Whinlatter compatibility: Files unpack to UNPACKDIR, not WORKDIR
do_install() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/swapfile-setup.service ${D}${systemd_system_unitdir}/swapfile-setup.service
    install -m 0644 ${UNPACKDIR}/data-swapfile.swap ${D}${systemd_system_unitdir}/data-swapfile.swap

    install -d ${D}${bindir}
    install -m 0755 ${UNPACKDIR}/swapfile-setup.sh ${D}${bindir}/swapfile-setup.sh
}

FILES:${PN} += "\
    ${systemd_system_unitdir}/swapfile-setup.service \
    ${systemd_system_unitdir}/data-swapfile.swap \
    ${bindir}/swapfile-setup.sh \
"

RDEPENDS:${PN} = "systemd"
