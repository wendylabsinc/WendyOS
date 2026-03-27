# dnsmasq configuration for NetworkManager integration.
# dnsmasq is pulled into the image only when WENDYOS_USB_NET_MODE = "dhcp-server"
# (via RDEPENDS in networkmanager_%.bbappend). This bbappend configures it for
# NM's method=shared mode: DBus support is required for NM to manage dnsmasq.
PACKAGECONFIG:append = " dbus"

# Disable the system-wide dnsmasq.service so it does not conflict with
# the NM-managed dnsmasq instance
SYSTEMD_AUTO_ENABLE = "disable"
