
DESCRIPTION = "USB gadget network extras: dnsmasq (DHCP on usb0) and Avahi (mDNS)"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://usb-gadget-dnsmasq.conf \
    file://gadget-dnsmasq.service \
    "

UNPACKDIR = "${UNPACKDIR}/sources"
S = "${UNPACKDIR}"

inherit systemd

RDEPENDS:${PN} += "dnsmasq avahi-daemon"

do_install() {
    # dnsmasq config dedicated to usb0
    install -d ${D}${sysconfdir}/dnsmasq.d
    install -m 0644 ${UNPACKDIR}/usb-gadget-dnsmasq.conf ${D}${sysconfdir}/dnsmasq.d/

    # Dedicated service so we don't alter any system-wide dnsmasq config
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/gadget-dnsmasq.service ${D}${systemd_system_unitdir}/
}

SYSTEMD_SERVICE:${PN} = "gadget-dnsmasq.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"
