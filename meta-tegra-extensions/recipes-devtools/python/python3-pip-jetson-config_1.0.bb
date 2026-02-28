SUMMARY = "Pip configuration for NVIDIA Jetson AI Lab PyPI index"
DESCRIPTION = "System-wide pip configuration that adds the Jetson AI Lab PyPI index for optimized wheels"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://pip.conf"

S = "${WORKDIR}"

do_install() {
    # Install pip.conf to system-wide pip configuration directory
    install -d ${D}${sysconfdir}
    install -m 0644 ${WORKDIR}/pip.conf ${D}${sysconfdir}/pip.conf
}

FILES:${PN} = "${sysconfdir}/pip.conf"

RDEPENDS:${PN} = "python3-pip"
