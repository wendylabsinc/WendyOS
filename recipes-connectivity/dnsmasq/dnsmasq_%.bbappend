# The USB gadget NM connection uses ipv4.method=auto (DHCP client mode).
# NM does not spawn dnsmasq in this mode; dnsmasq is present as a dependency
# of NetworkManager itself. DBus support is enabled for NM integration in case
# method=shared is used on other interfaces.
# NOTE: if method=auto is used exclusively, the dbus PACKAGECONFIG can be removed.
PACKAGECONFIG:append = " dbus"

# Disable the system-wide dnsmasq.service so it does not conflict with
# any NM-managed dnsmasq instances
SYSTEMD_AUTO_ENABLE = "disable"
