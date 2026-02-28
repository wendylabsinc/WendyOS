SUMMARY = "WendyOS First Boot Time Sync"
DESCRIPTION = "Forces time synchronization on first boot for WendyOS devices"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

SRC_URI = " \
    file://first-boot-timesync.service \
    "

S = "${WORKDIR}"

SYSTEMD_SERVICE:${PN} = "first-boot-timesync.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    # Install systemd service
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/first-boot-timesync.service ${D}${systemd_system_unitdir}/
}

FILES:${PN} += "${systemd_system_unitdir}/first-boot-timesync.service"

RDEPENDS:${PN} = "bash systemd"

COMPATIBLE_MACHINE = "rpi"
