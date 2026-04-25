SUMMARY = "CNI bridge network configuration for WendyOS containers"
DESCRIPTION = "CNI network configuration for WendyOS containers"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://10-bridge.conflist"

S = "${WORKDIR}"

# CNI naming convention: a config that uses the chained "plugins": [...] form
# (here: bridge + portmap) MUST end in .conflist. nerdctl's config parser
# rejects chained configs with a .conf extension ("missing 'type'").
do_install() {
    install -d ${D}${sysconfdir}/cni/net.d
    install -m 0644 ${WORKDIR}/10-bridge.conflist ${D}${sysconfdir}/cni/net.d/
}

FILES:${PN} = "${sysconfdir}/cni/net.d/*"

# This config is only needed when container runtime is enabled
inherit features_check
REQUIRED_DISTRO_FEATURES = "virtualization"
