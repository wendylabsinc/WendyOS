
PR = "r0"
PACKAGE_ARCH = "${MACHINE_ARCH}"

inherit packagegroup

SUMMARY:${PN} = "Base support"
RDEPENDS:${PN} = " \
    packagegroup-core-boot \
    bash \
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
    jq \
    k3s-agent \
    wendyos-identity \
    wendyos-agent \
    wendyos-user \
    wendyos-user-data-setup \
    wendyos-motd \
    systemd-mount-containerd \
    swapfile-setup \
    wendyos-etc-binds \
    containerd-config \
    xdg-dbus-proxy \
    "

# k3s-agent is excluded on RPi3/RPi4: those targets run containers via
# containerd directly (e.g. nerdctl), without Kubernetes orchestration.
RDEPENDS:${PN}:remove:raspberrypi3 = "k3s-agent"
RDEPENDS:${PN}:remove:raspberrypi4 = "k3s-agent"

RDEPENDS:${PN}:append = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEBUG') == '1', \
        ' \
            tcpdump \
            gzip \
        ', \
        '' \
        )} \
    "

# Include hardware-specific packagegroup configuration
require ${@'qemu-packagegroup-base.inc'  if 'qemuall' in d.getVar('MACHINEOVERRIDES').split(':') else ''}
require ${@'tegra-packagegroup-base.inc' if 'tegra'   in d.getVar('MACHINEOVERRIDES').split(':') else ''}
require ${@'packagegroup-base-rpi.inc'   if 'rpi'     in d.getVar('MACHINEOVERRIDES').split(':') else ''}
