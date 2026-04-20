SUMMARY = "WendyOS RPi-specific packages"
LICENSE = "MIT"
PACKAGE_ARCH = "${MACHINE_ARCH}"
inherit packagegroup

RDEPENDS:${PN} = " \
    wireless-regdb-static \
    expand-rootfs \
    first-boot-timesync \
    pi-bluetooth \
    "
# pi-bluetooth ships hciuart.service, which attaches the onboard BT radio to
# the system over UART on RPi3/4/5. Upstream meta-raspberrypi already pulls
# it in via RDEPENDS:bluez5:append:rpi, but declare it explicitly here so we
# don't silently lose BT if that bbappend ever changes.
# rpi-eeprom-config sets PSU_MAX_CURRENT, which is an RPi5-only EEPROM key
# (tied to BCM2712's PMIC). RPi4 has an EEPROM but PSU_MAX_CURRENT does not
# apply there; RPi3 has no EEPROM at all. The runtime script skips on non-RPi5.
# Include the package only on RPi5 to keep it out of RPi4/RPi3 builds.
RDEPENDS:${PN}:append:raspberrypi5 = " rpi-eeprom-config"

RDEPENDS:${PN}:append = " \
    ${@oe.utils.ifelse(d.getVar('WENDYOS_DEBUG') == '1', ' iw mmc-utils', '')} \
    "

COMPATIBLE_MACHINE = "rpi"
