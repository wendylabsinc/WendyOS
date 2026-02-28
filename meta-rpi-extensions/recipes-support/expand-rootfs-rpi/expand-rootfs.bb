SUMMARY = "First-boot rootfs auto-expansion service"
DESCRIPTION = "Expands the active root partition and grows the filesystem on first boot."
LICENSE = "CLOSED"

PR = "r4"

SRC_URI = " \
    file://expand-rootfs.sh \
    file://expand-rootfs.service \
"

S = "${WORKDIR}"

inherit systemd

do_install() {
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/expand-rootfs.sh ${D}${sbindir}/expand-rootfs.sh

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/expand-rootfs.service ${D}${systemd_system_unitdir}/expand-rootfs.service
}

FILES:${PN} += " \
    ${sbindir}/expand-rootfs.sh \
    ${systemd_system_unitdir}/expand-rootfs.service \
"

SYSTEMD_SERVICE:${PN} = "expand-rootfs.service"
SYSTEMD_AUTO_ENABLE = "enable"

RDEPENDS:${PN} = "bash coreutils util-linux parted e2fsprogs-resize2fs udev gptfdisk"

COMPATIBLE_MACHINE = "rpi"
