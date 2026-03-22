SUMMARY = "WendyOS installer payload import"
DESCRIPTION = "Imports installer-provided WendyOS configuration from the boot or EFI partition on first boot."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd allarch

SRC_URI = " \
    file://wendyos-install-config.sh \
    file://wendyos-install-config.service \
"

SYSTEMD_SERVICE:${PN} = "wendyos-install-config.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/wendyos-install-config.sh ${D}${sbindir}/wendyos-install-config.sh

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-install-config.service ${D}${systemd_system_unitdir}/wendyos-install-config.service
}

FILES:${PN} += " \
    ${sbindir}/wendyos-install-config.sh \
    ${systemd_system_unitdir}/wendyos-install-config.service \
"

RDEPENDS:${PN} = "bash jq shadow systemd"
