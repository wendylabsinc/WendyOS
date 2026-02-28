SUMMARY = "Raspberry Pi 5 EEPROM Power Configuration"
DESCRIPTION = "Configures Raspberry Pi 5 EEPROM PSU_MAX_CURRENT setting for USB gadget support"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://rpi5-eeprom-update.sh \
    file://rpi5-eeprom-config.service \
"

S = "${WORKDIR}"

RDEPENDS:${PN} = " \
    bash \
    coreutils \
    rpi-eeprom \
"

inherit systemd

SYSTEMD_SERVICE:${PN} = "rpi5-eeprom-config.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    # Install the EEPROM update script
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/rpi5-eeprom-update.sh ${D}${sbindir}/rpi5-eeprom-update.sh

    # Install the systemd service
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/rpi5-eeprom-config.service ${D}${systemd_system_unitdir}/rpi5-eeprom-config.service

    # Create directory for state files
    install -d ${D}${localstatedir}/lib/wendyos
}

FILES:${PN} += " \
    ${sbindir}/rpi5-eeprom-update.sh \
    ${systemd_system_unitdir}/rpi5-eeprom-config.service \
    ${localstatedir}/lib/wendyos \
"

# This package is only relevant for Raspberry Pi 5
COMPATIBLE_MACHINE = "rpi"
