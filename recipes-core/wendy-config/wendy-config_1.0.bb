SUMMARY = "WendyOS first-boot provisioning from WENDYCONFIG partition"
DESCRIPTION = "Locates the WENDYCONFIG FAT partition by volume label, reads \
wendy.conf into a tmpfs, wipes the original from the partition, dispatches \
handlers, and writes a stamp so the service is a no-op on every subsequent boot."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

SRC_URI = " \
    file://wendy-config.service \
    file://wendy-config.sh \
    file://wendy-config-lib.sh \
    "

SYSTEMD_SERVICE:${PN} = "wendy-config.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendy-config.service ${D}${systemd_system_unitdir}/wendy-config.service

    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/wendy-config.sh ${D}${bindir}/wendy-config.sh

    install -d ${D}${libdir}/wendy-config
    install -m 0644 ${WORKDIR}/wendy-config-lib.sh ${D}${libdir}/wendy-config/wendy-config-lib.sh
}

FILES:${PN} = " \
    ${systemd_system_unitdir}/wendy-config.service \
    ${bindir}/wendy-config.sh \
    ${libdir}/wendy-config/wendy-config-lib.sh \
    "

RDEPENDS:${PN} = "util-linux-findfs util-linux-mount"
