
FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Install NetworkManager configuration files
SRC_URI += " \
    file://NetworkManager.conf \
    file://00-manage-usb0.conf \
    file://10-usb-gadget.conf \
    file://usb-gadget.nmconnection \
    file://99-interface-metrics.conf \
    "

# Map WENDYOS_USB_NET_MODE to NetworkManager ipv4.method value
def usb_net_nm_method(d):
    mode = d.getVar('WENDYOS_USB_NET_MODE') or 'link-local'
    mapping = {'link-local': 'link-local', 'dhcp-client': 'auto', 'dhcp-server': 'shared'}
    if mode not in mapping:
        bb.warn("WENDYOS_USB_NET_MODE='%s' is not recognized, falling back to 'link-local'" % mode)
    return mapping.get(mode, 'link-local')

# dnsmasq is only needed for dhcp-server mode (NM spawns it for method=shared)
RDEPENDS:${PN}-daemon += "${@'dnsmasq' if d.getVar('WENDYOS_USB_NET_MODE') == 'dhcp-server' else ''}"

# Remove dnsmasq from NM's weak recommendations when not in dhcp-server mode
RRECOMMENDS:${PN}-daemon:remove = "${@'' if d.getVar('WENDYOS_USB_NET_MODE') == 'dhcp-server' else 'dnsmasq'}"

# Install main NetworkManager configuration
do_install:append() {
    # Install main config
    install -d ${D}${sysconfdir}/NetworkManager
    install -m 0644 ${WORKDIR}/NetworkManager.conf ${D}${sysconfdir}/NetworkManager/NetworkManager.conf

    # Install NetworkManager config drop-ins
    install -d ${D}${sysconfdir}/NetworkManager/conf.d
    install -m 0644 ${WORKDIR}/00-manage-usb0.conf ${D}${sysconfdir}/NetworkManager/conf.d/00-manage-usb0.conf
    install -m 0644 ${WORKDIR}/10-usb-gadget.conf ${D}${sysconfdir}/NetworkManager/conf.d/10-usb-gadget.conf
    install -m 0644 ${WORKDIR}/99-interface-metrics.conf ${D}${sysconfdir}/NetworkManager/conf.d/99-interface-metrics.conf

    # Install system connections (USB gadget profile)
    # Substitute @USB_NET_MODE@ placeholder with the NM method for WENDYOS_USB_NET_MODE
    install -d ${D}${sysconfdir}/NetworkManager/system-connections
    sed 's|@USB_NET_MODE@|${@usb_net_nm_method(d)}|' \
        ${WORKDIR}/usb-gadget.nmconnection \
        > ${D}${sysconfdir}/NetworkManager/system-connections/usb-gadget.nmconnection
    chmod 0600 ${D}${sysconfdir}/NetworkManager/system-connections/usb-gadget.nmconnection
}

# Make sure our config files are packaged
FILES:${PN} += " \
    ${sysconfdir}/NetworkManager/NetworkManager.conf \
    ${sysconfdir}/NetworkManager/conf.d/00-manage-usb0.conf \
    ${sysconfdir}/NetworkManager/conf.d/10-usb-gadget.conf \
    ${sysconfdir}/NetworkManager/conf.d/99-interface-metrics.conf \
    ${sysconfdir}/NetworkManager/system-connections/usb-gadget.nmconnection \
    "

# Ensure NetworkManager starts after USB gadget is set up
SYSTEMD_AUTO_ENABLE = "enable"
