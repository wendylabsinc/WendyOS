
SUMMARY = "Systemd drop-in so mender-updated waits for /boot/efi"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = " \
    file://10-requires-esp.conf \
    "
S = "${WORKDIR}"

inherit systemd

do_install() {
    install -d ${D}${systemd_system_unitdir}/mender-updated.service.d
    install -m 0644 ${WORKDIR}/10-requires-esp.conf \
        ${D}${systemd_system_unitdir}/mender-updated.service.d/10-requires-esp.conf
}

FILES:${PN} += "${systemd_system_unitdir}/mender-updated.service.d/10-requires-esp.conf"

# Drop-ins don't need explicit enablement; they are read automatically
SYSTEMD_AUTO_ENABLE:${PN} = "disable"
