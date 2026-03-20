SUMMARY = "USB gadget setup for composite NCM+ACM via configfs"
DESCRIPTION = "Configures a composite USB NCM+ACM gadget via configfs. \
    Supports Jetson (tegra-xudc), RPi5 (dwc2), and generic Linux USB controllers."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI = " \
    file://gadget-setup.sh \
    file://gadget-setup.service \
    file://wendyos-usbgadget-unbind.service \
    file://90-usb0-up.rules \
    file://99-usb-gadget-udc.rules \
    file://usb0-force-up \
    "
S = "${WORKDIR}"

inherit systemd

do_install() {
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/gadget-setup.sh ${D}${sbindir}/gadget-setup.sh

    install -d ${D}${libexecdir}
    install -m 0755 ${WORKDIR}/usb0-force-up ${D}${libexecdir}/usb0-force-up

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/gadget-setup.service ${D}${systemd_system_unitdir}/gadget-setup.service
    install -m 0644 ${WORKDIR}/wendyos-usbgadget-unbind.service ${D}${systemd_system_unitdir}/wendyos-usbgadget-unbind.service

    install -d ${D}${sysconfdir}/udev/rules.d
    install -m 0644 ${WORKDIR}/90-usb0-up.rules ${D}${sysconfdir}/udev/rules.d/90-usb0-up.rules
    install -m 0644 ${WORKDIR}/99-usb-gadget-udc.rules ${D}${sysconfdir}/udev/rules.d/99-usb-gadget-udc.rules
}

SYSTEMD_SERVICE:${PN} = "gadget-setup.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

RDEPENDS:${PN} += "bash udev kmod iproute2 systemd"

FILES:${PN} += " \
    ${sbindir}/gadget-setup.sh \
    ${libexecdir}/usb0-force-up \
    ${systemd_system_unitdir}/gadget-setup.service \
    ${systemd_system_unitdir}/wendyos-usbgadget-unbind.service \
    ${sysconfdir}/udev/rules.d/90-usb0-up.rules \
    ${sysconfdir}/udev/rules.d/99-usb-gadget-udc.rules \
    "
