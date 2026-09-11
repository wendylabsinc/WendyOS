# WendyOS Networking Overview

WendyOS provides a self-contained networking stack designed for USB-tethered connectivity between a device (Raspberry Pi, Jetson, or other supported hardware) and a host computer. When you plug a WendyOS device into a host via USB-C, the device presents a composite USB gadget that includes both a network interface and a serial console. No drivers, no pairing — the device advertises itself over mDNS and the wendy-agent can discover and connect to it immediately. On Linux hosts, run `wendy device usb-setup` once to configure the USB-C link.

## Architecture

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

## Key Addresses

| What | Value |
|---|---|
| Device `usb0` IPv4 (DHCP client mode) | Assigned by host (typically `10.42.0.2`–`10.42.0.5`) |
| Host IPv4 when using NetworkManager shared | `10.42.0.1/24` |
| dnsmasq pool (on-device DHCP server mode) | `10.42.0.2`–`10.42.0.5` |
| IPv6 | Link-local only (`fe80::/64`, derived from MAC via EUI-64) |
| mDNS service | `_wendyos._udp`, port `50051` pre-provisioning / `50052` post-provisioning |
| mDNS hostname | `wendyos-<device-name>.local` or `wendyos-<short-uuid>.local` |

## Platform Notes

WendyOS runs on multiple hardware platforms. Networking behaviour is identical across them, but the underlying USB controller differs:

| Platform | USB Controller | USB Version |
|---|---|---|
| Raspberry Pi 5 | `dwc2` | USB 2.0 |
| Jetson AGX Orin / Orin Nano | `tegra-xudc` | USB 3.2 |
| Other Linux (generic) | Detected at runtime | USB 2.0 fallback |

The virtual machine boards (`vm-arm64-wendyos`, `vm-x86-64-wendyos`) do not use USB NCM gadget networking. They emulate a standard virtio network device instead.

## Planned changes (not yet shipped)

> The items below describe behaviour that is **not implemented today**. Current builds still rely exclusively on NetworkManager via its `nmcli` CLI for WiFi configuration, and have no built-in mTLS termination on the device side.

- **Network Manager and IWD coexistence via auto-detect** ([WDY-1236](https://linear.app/wendylabsinc/issue/WDY-1236)) — _planned, not yet implemented_. Today the agent talks to NetworkManager (`nmcli`) only. The planned change adds IWD (Internet Wireless Daemon) as an alternative. The agent will detect which CLI is present on the running OS and select the matching backend, so devices on older OS builds that ship NetworkManager keep working unchanged. The OS-side change to ship `iwd` lands first; the agent-side switchover is gated on the OS PR.
- **Unified TCP-based socket protocol for CLI and cloud** ([WDY-1246](https://linear.app/wendylabsinc/issue/WDY-1246)) — _planned, not yet implemented_. The same low-level TCP socket protocol — messages standardised by size, type, and payload — will back both cloud-to-device and CLI-to-device sessions, with the `wendy` CLI behaving as a local cloud instance. The existing flow (CLI emits a UDP broadcast, the device opens an HTTP connection back) becomes one of the supported transports under that unified surface.
- **Mutual TLS for device communication** ([WDY-1245](https://linear.app/wendylabsinc/issue/WDY-1245) covers the Wendy Light provisioning slice) — _planned, not yet implemented_. Sample mTLS socket plumbing exists in a development branch and will be merged once the unified protocol above is in place. The accompanying provisioning improvements (generating private keys and CSRs directly on-device, mDNS-based discovery) follow.
