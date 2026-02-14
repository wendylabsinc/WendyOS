SUMMARY = "Systemd mount unit for persistent containerd data directory"
DESCRIPTION = "Bind mounts /var/lib/containerd from /data/containerd to provide persistent \
container images, volumes, and snapshots across Mender OTA updates. Ensures containers \
and their data survive A/B partition switches."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = "file://var-lib-containerd.mount"

SYSTEMD_SERVICE:${PN} = "var-lib-containerd.mount"
SYSTEMD_AUTO_ENABLE = "enable"

# Whinlatter compatibility: Files unpack to UNPACKDIR, not WORKDIR
do_install() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/var-lib-containerd.mount ${D}${systemd_system_unitdir}/var-lib-containerd.mount
}

FILES:${PN} += "${systemd_system_unitdir}/var-lib-containerd.mount"

RDEPENDS:${PN} = "systemd"
