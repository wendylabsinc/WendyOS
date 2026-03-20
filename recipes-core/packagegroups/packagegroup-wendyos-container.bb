SUMMARY = "WendyOS container runtime support"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

PACKAGE_ARCH = "${MACHINE_ARCH}"
inherit packagegroup

# Core container runtime packages
RDEPENDS:${PN} = " \
    containerd-opencontainers \
    runc-opencontainers \
    cni \
    cni-config \
    iptables \
    iptables-modules \
    ca-certificates \
    ${@bb.utils.contains('DISTRO_FEATURES', 'systemd', 'systemd-container', '', d)} \
    "

# Essential kernel modules for container networking and isolation.
# RRECOMMENDS is used because these may be built into the kernel (=y) rather
# than as loadable modules (=m); missing them is non-fatal.
RRECOMMENDS:${PN} = " \
    kernel-module-overlay \
    kernel-module-bridge \
    kernel-module-br-netfilter \
    kernel-module-veth \
    kernel-module-xt-nat \
    kernel-module-xt-masquerade \
    kernel-module-xt-conntrack \
    kernel-module-xt-addrtype \
    kernel-module-nf-nat \
    kernel-module-nf-conntrack \
    kernel-module-ip-tables \
    "
