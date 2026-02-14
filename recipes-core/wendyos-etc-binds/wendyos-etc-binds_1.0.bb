
SUMMARY = "Bind mount persistent /etc files from /data partition"
DESCRIPTION = "Systemd service to bind-mount identity and configuration files from \
/data/etc/ to /etc/ for persistence across Mender OTA updates. Ensures device UUID, \
hostname, and network configurations persist when switching between A/B rootfs slots."
LICENSE = "Apache-2.0"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/Apache-2.0;md5=89aea4e17d99a7cacdbeed46a0096b10"

inherit systemd

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = " \
    file://wendyos-etc-binds.service \
    file://setup-etc-binds.sh \
    "

SYSTEMD_SERVICE:${PN} = "wendyos-etc-binds.service"
SYSTEMD_AUTO_ENABLE = "enable"

RDEPENDS:${PN} += "bash"

do_install() {
    # Install systemd service unit
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/wendyos-etc-binds.service ${D}${systemd_system_unitdir}/

    # Install bind mount setup script
    install -d ${D}${sbindir}
    install -m 0755 ${UNPACKDIR}/setup-etc-binds.sh ${D}${sbindir}/
}

FILES:${PN} += "${systemd_system_unitdir}/wendyos-etc-binds.service"
