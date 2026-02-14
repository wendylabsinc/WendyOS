SUMMARY = "Systemd mount unit for persistent /home directory"
DESCRIPTION = "Bind mounts /home from /data/home to provide persistent user home directories across OTA updates"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = "file://home.mount"

SYSTEMD_SERVICE:${PN} = "home.mount"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/home.mount ${D}${systemd_system_unitdir}/home.mount
}

FILES:${PN} += "${systemd_system_unitdir}/home.mount"

RDEPENDS:${PN} = "systemd"
