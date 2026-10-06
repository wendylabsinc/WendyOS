# Networking Troubleshooting

This guide covers common WendyOS USB networking problems, their symptoms, and how to diagnose and fix them.

## Connectivity Checklist

Run through this before diving into specific scenarios:

```
[ ] USB-C data cable connected (not a charge-only cable)
[ ] Board booted — serial console available: screen /dev/ttyACM0
[ ] Host sees gadget interface:       ip link show | grep enx
[ ] Host sharing / link-local active: ip addr show enxXXXXXXXXXXXX
[ ] Device has address on usb0:       ip addr show usb0   (run on device)
[ ] Gadget service healthy:           systemctl status gadget-setup
[ ] ARP resolved:                     ip neigh show dev usb0  (on device)
[ ] Ping host from device:            ping -c 3 10.42.0.1
[ ] Ping device from host:            ping -c 3 <device-ip>
[ ] mDNS visible from host:           avahi-browse -t -p _wendyos._udp
```

---

## Scenario 1 — Host does not see the gadget interface (`enx` interface missing)

**Symptom:** `ip link show | grep enx` produces no output after plugging in.

**Step 1 — check if the USB device is enumerated at all:**
```bash
# On host
lsusb | grep "1d6b:0104"
# Expected: Bus 00X Device 00Y: ID 1d6b:0104 Linux Foundation Multifunction Composite Gadget
```

If the device is not listed:
- Try a different cable (data cable, not charge-only)
- Try a different USB port
- Check if the device has booted — connect via a USB-C power supply and watch the serial console

**Step 2 — if lsusb shows the device but no network interface:**
```bash
# On host
dmesg | grep -i "cdc_ncm\|cdc_ether\|usb"
```
Look for driver binding messages. If you see `cdc_ncm` being loaded, the interface should appear. If not, the module may not be available or another driver grabbed the device.

**Step 3 — check gadget-setup status on the device (via serial console):**
```bash
# On device (/dev/ttyACM0)
systemctl status gadget-setup
journalctl -u gadget-setup --no-pager
```

Common gadget-setup failures:
- "UDC timeout after 60 s" — the UDC module has not loaded; check `lsmod | grep -E "tegra_xudc|dwc2|dwc3"`
- "Neither NCM nor ECM is available" — gadget function modules failed to load; check `dmesg | grep usb_f_ncm`

---

## Scenario 2 — Device gets no IPv4 address

**Symptom:** `ip addr show usb0` shows only an IPv6 link-local address, no IPv4.

**Step 1 — verify the host is providing DHCP:**
```bash
# On host
ip addr show enxXXXXXXXXXXXX    # should show 10.42.0.1/24 if NM sharing is active
nmcli connection show | grep -i usb
```

If the host is not set up for sharing, the simplest fix on Linux is to run
`wendy discover` and accept the offered USB setup — it configures a link-local
NetworkManager profile bound to the gadget's MAC (so it survives a change of USB
port) and installs a udev rule so ModemManager leaves the gadget alone. It does
not share internet; use the NM shared profile below for that.

The equivalent manual steps:
```bash
sudo ./scripts/setup-host-usb-link-local.sh install
# Or for full internet sharing (NM):
nmcli connection add type ethernet ifname enxXXXXXXXXXXXX con-name usb-gadget-sharing ipv4.method shared
nmcli connection up usb-gadget-sharing
```

To remove what the CLI installed: `nmcli connection delete wendy-usb-<mac>` and
`sudo rm -f /etc/udev/rules.d/99-wendy-usb.rules`.

**Step 2 — check NetworkManager on the device:**
```bash
# On device
journalctl -u NetworkManager | grep -i "usb0\|dhcp"
nmcli connection show usb-gadget | grep -E "ipv4|IP4|STATE"
```

**Step 3 — force a new DHCP attempt:**
```bash
# On device
nmcli connection down usb-gadget && nmcli connection up usb-gadget
```

**Step 4 — capture DHCP traffic:**
```bash
# On host
sudo tcpdump -i enxXXXXXXXXXXXX port 67 or port 68 -vn
```

Expected: `DISCOVER → OFFER ← REQUEST → ACK ←`

If you see repeated DISCOVERs with no OFFER: the host DHCP server is not active or bound to a different interface. If you see OFFER but no REQUEST: the device is not receiving the OFFER — this is often a receive-path problem (see Scenario 3).

---

## Scenario 3 — IP assigned but packets lost (usb0 has address, ping fails)

**Symptom:** `usb0` has an IP address but `ping 10.42.0.1` gets 100% packet loss.

**Step 1 — check RX/TX counters:**
```bash
# On device
ip -s link show usb0
```
If TX is in the hundreds but RX is near zero, the USB receive path is broken. This is characteristic of the DWC2 DMA stall bug on RPi5 (see below).

**Step 2 — check ARP resolution:**
```bash
# On device
ip neigh show dev usb0
# REACHABLE = ARP is working
# INCOMPLETE = ARP is stuck; the device sent a request but got no reply back

# On host — watch ARP
sudo tcpdump -i enxXXXXXXXXXXXX arp -n
```

If the host sends ARP Replies but the device still shows INCOMPLETE, packets are being dropped between the USB layer and the network stack.

**Step 3 — check packet filters:**
```bash
# On device
nft list ruleset
iptables -L -n -v
sysctl net.ipv4.conf.usb0.rp_filter
```

A non-zero `rp_filter` can drop packets if the route is asymmetric.

**Step 4 — inspect the USB endpoint (RPi5/DWC2 specific):**
```bash
# On device
cat /sys/kernel/debug/usb/udc/1000480000.usb/ep1out
```

Healthy output has `NAKSts=0` and requests completing with non-zero `total_data`.

Broken output (DMA stall):
```
NAKSts=1                          # hardware is NAKing all OUT tokens from host
DOEPDMA=0xXXXXXXXX               # DMA address programmed but never completing
req: ... EINPROGRESS total_data=0 # all requests stuck
```

**Root cause (RPi5, kernel 6.6, BCM2712):** BCM2712 reports `arch=2` (DMA capable) but DMA completion interrupts never fire for OUT transfers in peripheral mode. Fix: `g_dma=false` in `dwc2_set_bcm_params()` — this patch is applied in `meta-rpi-extensions/`. **Do not write `""` to the UDC sysfs file on an unpatched board** — this triggers `ep_stop_xfr` timeouts and leaves Global OUT NAK permanently asserted. Reboot instead.

---

## Scenario 4 — mDNS device not visible on host

**Symptom:** `avahi-browse -t -p _wendyos._udp` returns no results.

**Step 1 — check Avahi is running on the device:**
```bash
# On device
systemctl status avahi-daemon
journalctl -u avahi-daemon --no-pager | tail -30
```

**Step 2 — check that the service file was populated:**
```bash
# On device
cat /etc/avahi/services/wendyos-mdns.service
```
The file should contain the actual UUID, not the placeholder string `WENDY_DEVICE_ID`. If placeholders are present, `update-mdns-uuid.sh` did not run or failed:
```bash
journalctl -t wendyos-identity --no-pager
ls /etc/wendyos/device-uuid /etc/wendyos/device-name
```

**Step 3 — check UUID and hostname services:**
```bash
# On device
systemctl status wendyos-hostname.service
systemctl status wendyos-uuid-generate.service    # if it exists on this build
```

**Step 4 — check mDNS multicast is reaching the host:**
```bash
# On host
sudo tcpdump -i enxXXXXXXXXXXXX udp port 5353 -vn
```

You should see mDNS queries and responses. If you see nothing, multicast may be blocked on the link.

**Step 5 — test with avahi-browse on the device itself:**
```bash
# On device
avahi-browse -t _wendyos._udp
```
If the device sees its own service, the problem is on the host side. Check that `avahi-daemon` or `dns-sd` is running on the host and that the NCM interface is not isolated from multicast.

---

## Scenario 5 — NM stale connection profile on host

**Symptom:**
```
Warning: There is another connection with the name 'usb-gadget-sharing'.
Error: Connection activation failed: No suitable device found for this
connection (device docker0 not available ...)
```

A previous NetworkManager profile is left over and matches the wrong interface.

**Fix:**
```bash
# Delete all stale profiles with that name
nmcli -g UUID,NAME connection show | grep ':usb-gadget-sharing$' | \
    cut -d: -f1 | xargs -I{} sudo nmcli connection delete {}

# Then re-enable
sudo nmcli connection add type ethernet ifname enxXXXXXXXXXXXX \
    con-name usb-gadget-sharing ipv4.method shared
sudo nmcli connection up usb-gadget-sharing
```

---

## Scenario 6 — gadget-setup fails on Jetson (UDC not found / stays in default state)

**Symptom:** `journalctl -u gadget-setup` shows the UDC was found but the gadget does not appear on the host, or the role-switch warning is logged.

Jetson relies on the FUSB301 CC chip and the `tegra_xudc` driver. If the padctl role-switch notifier chain has not fired before `gadget-setup.sh` runs, the UDC stays in "default" state and never enters device mode.

The script attempts to force device mode:
```bash
echo "device" > /sys/class/usb_role/usb2-0-role-switch/role
```

Check whether this succeeded:
```bash
# On device
journalctl -u gadget-setup | grep -i "role"
cat /sys/class/usb_role/usb2-0-role-switch/role    # should show "device"

# Check UDC state
cat /sys/kernel/config/usb_gadget/wendyos_device/UDC
```

If the UDC path differs (AGX-style topology):
```bash
ls /sys/class/usb_role/
# Look for usb2-*-role-switch
```

---

## Scenario 7 — host-network container cannot resolve names after a network change

**Symptom:** A container using network mode `host` can reach IP addresses but
cannot resolve hostnames, even though the host can. Restarting or redeploying
the container temporarily fixes it.

Host-network containers created while the systemd-resolved stub is available
use a Wendy-managed `/etc/resolv.conf` that points to `127.0.0.53`. Because the
container shares the host network namespace, its queries reach the live host
resolver and automatically follow later upstream-DNS server changes from DHCP,
VPN, or WiFi reconfiguration. Search domains known to systemd-resolved are
copied into the managed file when the container is deployed and whenever it
starts after a reboot. Restart the app to pick up a search-domain change made
while it is already running; fully qualified names continue to follow upstream
changes without a restart.

Check the host and container:

```bash
# On the device
systemctl is-active systemd-resolved
ss -lnt 'sport = :53'

# Inside the host-network container
cat /etc/resolv.conf
# Expected primary path: nameserver 127.0.0.53
```

When systemd-resolved is absent or its stub listener is disabled, wendy-agent
keeps compatibility with other Linux hosts by bind-mounting the host resolver
file. The agent logs `systemd-resolved stub is unavailable` when it takes this
fallback. A running fallback container cannot observe an atomic replacement of
that file; restart the app after the host resolver changes, or enable
systemd-resolved for live updates.

Containers created by an older agent retain their existing OCI mount. Redeploy
them once after upgrading the agent so they receive the managed stub
configuration.

---

## Reference — All Diagnostic Commands

### On the device

```bash
# Gadget setup
systemctl status gadget-setup
journalctl -u gadget-setup --no-pager

# Network interface
ip addr show usb0
ip -s link show usb0
ip neigh show dev usb0
ip neigh flush dev usb0

# NetworkManager
systemctl status NetworkManager
journalctl -u NetworkManager | grep -i usb0
nmcli connection show usb-gadget
nmcli connection down usb-gadget && nmcli connection up usb-gadget

# Avahi / mDNS
systemctl status avahi-daemon
cat /etc/avahi/services/wendyos-mdns.service
avahi-browse -t _wendyos._udp
avahi-daemon --check

# radvd
systemctl status radvd
ip -6 addr show usb0

# USB hardware (RPi5/DWC2)
cat /sys/kernel/debug/usb/udc/1000480000.usb/ep1out
ls /sys/class/udc/
cat /sys/kernel/config/usb_gadget/wendyos_device/UDC
ls /sys/kernel/config/usb_gadget/wendyos_device/functions/
dmesg | grep dwc2
dmesg | grep -iE "usb|dwc|ncm|udc" | tail -40

# USB hardware (Jetson)
cat /sys/class/usb_role/usb2-0-role-switch/role
ls /sys/class/usb_role/

# Packet filters
nft list ruleset
iptables -L -n -v
sysctl net.ipv4.conf.usb0.rp_filter
```

### On the host

```bash
# Gadget detection
lsusb | grep "1d6b:0104"
ip link show | grep enx
dmesg | grep -i "cdc_ncm\|ncm\|ether"

# Interface state
ip addr show enxXXXXXXXXXXXX
ip -s link show enxXXXXXXXXXXXX
ip -6 addr show enxXXXXXXXXXXXX

# ARP
ip neigh show dev enxXXXXXXXXXXXX
sudo tcpdump -i enxXXXXXXXXXXXX arp -n

# DHCP
sudo tcpdump -i enxXXXXXXXXXXXX port 67 or port 68 -vn

# mDNS
avahi-browse -t -p _wendyos._udp      # Linux
dns-sd -B _wendyos._udp local.        # macOS
sudo tcpdump -i enxXXXXXXXXXXXX udp port 5353 -vn

# NetworkManager
nmcli connection show | grep -i usb
nmcli -g UUID,NAME connection show | grep usb-gadget-sharing

# Serial console (fallback)
screen /dev/ttyACM0
picocom /dev/ttyACM0

# SSH
ssh root@<device-ip>
ssh root@wendyos-<name>.local
```
