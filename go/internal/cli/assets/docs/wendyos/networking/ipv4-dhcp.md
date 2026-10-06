# IPv4 DHCP

WendyOS assigns an IPv4 address to `usb0` through DHCP. There are two modes depending on how the host is set up.

## Modes

### Mode 1 — DHCP Client (Default)

In the default build configuration (`WENDYOS_USB_NET_MODE=dhcp`), WendyOS acts as a DHCP client on `usb0`. The host is expected to run a DHCP server on its NCM adapter. The most common way to do this on Linux is to activate a NetworkManager "shared" connection on the host-side interface, which starts a dnsmasq instance automatically and assigns `10.42.0.1/24` to the host.

The NetworkManager connection profile on the device (`/etc/NetworkManager/system-connections/usb-gadget.nmconnection`) sets:
```ini
[ipv4]
method=@USB_NET_MODE@     # replaced at build time with "dhcp"
route-metric=700          # high metric, usb0 is the fallback route
```

The high route metric (`700`) means that Ethernet (`100`) and WiFi (`300`) are always preferred as the default route when available. `usb0` only becomes the default route if no other interface is up.

DHCP timeout is set to 300 seconds (`ipv4.dhcp-timeout=300`) to handle hosts that activate sharing after the device has already booted.

### Mode 2 — On-Device DHCP Server (Optional)

An alternative configuration ships an on-device dnsmasq instance that serves DHCP directly on `usb0`. This is provided by the `gadget-network-config` Yocto recipe.

Configuration file: `/etc/dnsmasq.d/usb-gadget-dnsmasq.conf`

```
interface=usb0
bind-interfaces

dhcp-range=10.42.0.2,10.42.0.5,12h

dhcp-option=3,10.42.0.1    # gateway = device itself
dhcp-option=6,8.8.8.8      # DNS

quiet-dhcp
quiet-dhcp6
quiet-ra
```

The systemd service `gadget-dnsmasq.service` starts after `gadget-setup.service` and runs dnsmasq with this config. It is type `simple` and restarts on failure.

In this mode the host receives an address from `10.42.0.2`–`10.42.0.5` and the device is the gateway at `10.42.0.1`. This requires the device to be configured with a static `10.42.0.1/24` address on `usb0`, which is done by setting `WENDYOS_USB_NET_MODE=shared` at build time.

## NetworkManager Configuration Files

| File | Purpose |
|---|---|
| `/etc/NetworkManager/system-connections/usb-gadget.nmconnection` | Per-interface connection profile for `usb0` |
| `/etc/NetworkManager/conf.d/10-usb-gadget.conf` | Ensures `usb0` is managed; sets `ipv6.method=link-local` |
| `/etc/NetworkManager/conf.d/00-manage-usb0.conf` | Explicitly clears `unmanaged-devices` so NM manages all interfaces |
| `/etc/NetworkManager/conf.d/99-interface-metrics.conf` | Sets route metrics: ethernet=100, wifi=300, usb=700 |

## Host-Side Setup

### Easiest — let `wendy discover` set it up (Linux)

On a Linux host, `wendy discover` auto-detects a USB-C-tethered Wendy device
whose host link isn't configured and offers to set it up for you. Accept the
prompt and enter your sudo password; the CLI brings the gadget interface up via
a link-local NetworkManager profile (IPv4 `169.254.0.0/16` and IPv6 `fe80::/64`,
no DHCP, matching the device's default link-local mode) and installs a udev rule
so ModemManager stops grabbing the gadget's serial console. The profile is named
`wendy-usb-<mac>` and bound to the gadget's MAC address rather than an interface
name, so it keeps working when you plug the device into another USB port. Each
device gets its own profile.

To undo it later, remove the profile and rule manually:

```sh
nmcli -f NAME connection show | grep wendy-usb   # find the profile
sudo nmcli connection delete wendy-usb-<mac>
sudo rm -f /etc/udev/rules.d/99-wendy-usb.rules
```

The CLI setup does not share the host's internet with the device. For that, use
the manual shared-mode steps below.

### Mode 1 host setup — NetworkManager shared

Run on the host once the NCM interface (`enxXXXXXXXXXXXX`) appears:

```sh
nmcli connection add type ethernet ifname <enxXXX> ipv4.method shared
```

This activates a built-in dnsmasq instance, assigns `10.42.0.1/24` to the host, and enables NAT so the device can reach the internet through the host.

### Alternative — link-local addressing

The `setup-host-usb-link-local.sh` script in the `wendyos` repository configures the host for link-local addressing instead. This is useful when internet sharing is not needed. It supports both NetworkManager and systemd-networkd:

**NetworkManager** — installs `/etc/NetworkManager/system-connections/wendyos-usb.nmconnection`:
```ini
[connection]
type=ethernet
autoconnect-priority=100
match.driver=cdc_ncm;cdc_ether

[ipv4]
method=link-local
```

**systemd-networkd** — installs `/etc/systemd/network/80-wendyos-usb.network`:
```ini
[Match]
Driver=cdc_ncm cdc_ether

[Network]
LinkLocalAddressing=ipv4
```

## Verifying DHCP

```bash
# On the device — check address assigned to usb0
ip addr show usb0

# On the device — check NetworkManager state
nmcli connection show usb-gadget | grep -E "ipv4|IP4"

# On the device — watch DHCP exchange in journal
journalctl -u NetworkManager | grep -i "usb0\|dhcp"

# On the host — capture DHCP packets
sudo tcpdump -i enxXXXXXXXXXXXX port 67 or port 68 -vn
```

A successful DHCP exchange looks like:
```
DISCOVER → (device to host, broadcast)
         ← OFFER    (host to device)
REQUEST  → (device to host)
         ← ACK      (host to device)
```
