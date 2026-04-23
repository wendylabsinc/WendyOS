# Enables hardware USB gadget (ECM/NCM). Include only for boards with USB gadget hardware.

WENDYOS_USB_GADGET = "1"

# USB gadget network mode (requires WENDYOS_USB_GADGET = "1"):
#   "link-local"  - Self-assigned 169.254.x.x, discovery via mDNS/Avahi (default)
#   "dhcp-client" - Device is DHCP client, host must run DHCP server
#   "dhcp-server" - Device runs DHCP server (NM shared mode, requires dnsmasq)
# Uncomment to override:
#WENDYOS_USB_NET_MODE = "link-local"

# Avahi mDNS interface restriction:
#   "usb0" - Only discoverable via USB (default)
#   ""     - Discoverable on all interfaces (including LAN)
# Uncomment to override:
#WENDYOS_MDNS_INTERFACES = "usb0"
