
PR = "r0"
PACKAGE_ARCH = "${MACHINE_ARCH}"

inherit packagegroup

SUMMARY:${PN} = "Base support"
RDEPENDS:${PN} = " \
    packagegroup-core-boot \
    bash \
    efibootmgr \
    coreutils \
    libstdc++ \
    file \
    util-linux \
    iproute2 \
    lsof \
    networkmanager \
    networkmanager-nmcli \
    vim \
    htop \
    usbutils \
    tree \
    util-linux-fdisk \
    avahi-daemon \
    avahi-wendyos-hostname \
    avahi-utils \
    k3s-agent \
    wendyos-identity \
    wendyos-etc-binds \
    wendyos-agent \
    wendyos-user \
    wendyos-motd \
    systemd-mount-containerd \
    swapfile-setup \
    containerd-config \
    tegra-tools-tegrastats \
    "

RDEPENDS:${PN}:append = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEBUG') == '1', \
        ' \
            \
        ', \
        '' \
        )} \
    "
