# Remove packages that require Mender A/B partitions (/data partition)
RDEPENDS:${PN}:remove:rpi = " \
    wendyos-etc-binds \
    systemd-mount-containerd \
    swapfile-setup \
    wendyos-user \
    "

# Add RPi5-specific kernel modules
RDEPENDS:${PN}:append:rpi = " \
    kernel-module-option \
    kernel-module-usb-wwan \
    kernel-module-usbserial \
    "
