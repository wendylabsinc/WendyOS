SUMMARY = "Containerd global memory limit configuration"
DESCRIPTION = "Configures containerd with a global 7.4GB memory limit for ALL containers combined to prevent device lockup"

inherit systemd
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = "\
    file://containerd-memory-limit.conf \
    file://README-memory-limits.md \
"

do_install() {
    # Install systemd drop-in to limit total containerd memory
    install -d ${D}${systemd_system_unitdir}/containerd.service.d
    install -m 0644 ${UNPACKDIR}/containerd-memory-limit.conf ${D}${systemd_system_unitdir}/containerd.service.d/memory-limit.conf

    install -d ${D}${docdir}/${PN}
    install -m 0644 ${UNPACKDIR}/README-memory-limits.md ${D}${docdir}/${PN}/README-memory-limits.md
}

FILES:${PN} += "\
    ${systemd_system_unitdir}/containerd.service.d/memory-limit.conf \
    ${docdir}/${PN}/README-memory-limits.md \
"

# Note: No RDEPENDS needed - this just provides config files
# containerd-opencontainers will be pulled in by other packages (k3s-agent, etc.)
