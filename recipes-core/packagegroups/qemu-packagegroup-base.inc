# QEMU-specific packagegroup-wendyos-base configuration
# This file is included when building for QEMU machines

# Remove data partition dependent services for QEMU
# QEMU uses single-partition layout without separate /data
RDEPENDS:${PN}:remove = " \
    wendyos-user-data-setup \
    wendyos-etc-binds \
    swapfile-setup \
    systemd-mount-containerd \
    "
