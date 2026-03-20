SUMMARY = "WendyOS RPi5-specific packages"
LICENSE = "MIT"
PACKAGE_ARCH = "${MACHINE_ARCH}"
inherit packagegroup

RDEPENDS:${PN} = " \
    rpi-eeprom-config \
    wireless-regdb-static \
    expand-rootfs \
    first-boot-timesync \
    "
RDEPENDS:${PN}:append = " \
    ${@oe.utils.ifelse(d.getVar('WENDYOS_DEBUG') == '1', ' iw mmc-utils', '')} \
    "

COMPATIBLE_MACHINE = "rpi"
