# WendyOS networking overview

WendyOS supports device management over USB, Ethernet, and Wi-Fi, plus
application communication through Wendy Net. Choose the workflow you need:

- [Wendy Net](/docs/guides/wendy-net) connects enrolled devices over enabled
  local transports, including routes through intermediate devices. Internet
  sharing and roaming are separate opt-ins.
- [App service discovery](/docs/guides/wendy-net-services) lets isolated apps
  advertise and browse ordinary mDNS services across Wendy Net. TCP and UDP
  access requires explicit port mappings.
- [USB NCM](./usb-ncm.md) provides a direct management connection. On Linux,
  `wendy discover` offers the one-time host link setup when needed.
- [mDNS and authentication](./mdns.md) explains LAN discovery. Advertisements
  are routing hints; enrolled devices authenticate with mTLS on port 50052.

The USB-specific sections below describe the gadget network, not the entire
Wendy Net address space. Wendy Net service apps use `10.99.0.0/16`; applications
must use locally resolved addresses rather than infer addresses from device IDs.

## USB gadget architecture

On the Dragonwing IQ8, use USB2 (micro-USB) for this connection after WendyOS boots.
USB0 (USB-C) is reserved for EDL/recovery flashing. The port assignment is the
same with the stock Qualcomm or a replacement Intel M.2 card.

```
WendyOS device                          Host computer
───────────────────────────────         ───────────────────────────────
  usb0 (NCM gadget interface)  ←USB→    enxXXXXXXXXXXXX (CDC NCM)
  IPv4: DHCP client (or shared)         IPv4: 10.42.0.1/24 (NM shared)
  IPv6: link-local (auto)               IPv6: link-local (auto)
  mDNS: _wendyos._udp (Avahi)           mDNS discovery (avahi-browse/dns-sd)
  /dev/ttyGS0 (ACM serial)     ←USB→    /dev/ttyACM0 (serial console)
```

## Components

| Component | Role | Source |
|---|---|---|
| `gadget-setup.sh` | Configures the USB gadget in configfs at boot | `recipes-core/gadget-setup/` |
| systemd-networkd | Manages `usb0` IP configuration | `recipes-core/systemd/` |
| dnsmasq | Optional on-device DHCP server for `usb0` | `recipes-support/gadget-network-config/` |
| radvd | IPv6 Router Advertisement daemon on `usb0` | `recipes-connectivity/radvd/` |
| Avahi | mDNS service broadcasting (`_wendyos._udp`) | `recipes-connectivity/avahi/` |
| `generate-hostname.sh` | Sets a stable device hostname before Avahi starts | `recipes-connectivity/avahi/` |
| `update-mdns-uuid.sh` | Injects device UUID and name into the Avahi service file | `recipes-core/wendyos-identity/` |

## Topics

- [USB NCM Gadget](./usb-ncm.md) — How the USB gadget is constructed and initialized
- [MAC Address Generation](./mac-addresses.md) — Deterministic MAC generation and IPv6 link-local formation
- [IPv4 DHCP](./ipv4-dhcp.md) — DHCP client and server modes for `usb0`, and host-side setup
- [IPv6 Router Advertisements](./ipv6-ra.md) — radvd configuration for IPv6 link-local on `usb0`
- [mDNS / Avahi](./mdns.md) — Service broadcasting, hostname generation, and host-side discovery
- [Troubleshooting](./troubleshooting.md) — Diagnosing common networking problems

## USB management addresses

| What | Value |
|---|---|
| Device `usb0` IPv4 (DHCP client mode) | Assigned by host (typically `10.42.0.2`–`10.42.0.5`) |
| Host IPv4 when using NetworkManager shared | `10.42.0.1/24` |
| dnsmasq pool (on-device DHCP server mode) | `10.42.0.2`–`10.42.0.5` |
| USB IPv6 | Link-local (`fe80::/64`, derived from MAC via EUI-64) |
| mDNS service | `_wendyos._udp`, port `50051` pre-provisioning / `50052` post-provisioning |
| mDNS hostname | `wendyos-<device-name>.local` or `wendyos-<short-uuid>.local` |

## Platform Notes

WendyOS runs on multiple hardware platforms. Networking behaviour is identical across them, but the underlying USB controller differs:

| Platform | USB Controller | USB Version |
|---|---|---|
| Raspberry Pi 5 | `dwc2` | USB 2.0 |
| Jetson AGX Orin / Orin Nano | `tegra-xudc` | USB 3.2 |
| Dragonwing IQ8 | `a400000.usb` | USB 2.0 |
| Other Linux (generic) | Detected at runtime | USB 2.0 fallback |

The virtual machine boards (`vm-arm64-wendyos`, `vm-x86-64-wendyos`) do not use USB NCM gadget networking. They emulate a standard virtio network device instead.

## Current control paths

The CLI uses the agent's gRPC service for management, including mTLS for enrolled
devices. Wendy Net has its own authenticated peer connections and app service
publication. Device discovery, management authentication, and app discovery are
separate; finding a device does not publish every container port.

Wi-Fi configuration currently uses NetworkManager through `nmcli`. Wendy Net
participation and transport settings use `wendy net configure`; they do not
replace `wendy device wifi connect` for joining an ordinary Wi-Fi network.
See [Wi-Fi setup](/docs/device/connecting-to-wifi) and the
[Wendy Net settings reference](/docs/guides/wendy-net#settings-reference).
