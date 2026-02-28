
SUMMARY = "Systemd drop-in so mender-updated waits for /boot/efi"
LICENSE = "MIT"
# LIC_FILES_CHKSUM = "file://10-requires-esp.conf;md5=1f3870be274f6c49b3e31a0c6728957f"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=${@bb.utils.md5_file(d.getVar('COMMON_LICENSE_DIR') + '/MIT')}"

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
