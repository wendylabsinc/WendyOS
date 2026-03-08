# RPi5 USB Gadget Troubleshooting

---

## Hardware Setup

| Side | Detail |
|---|---|
| RPi5 port | USB-C port (top-left, the only USB-C on the board) |
| Host port | Any USB-A or USB-C port on the Linux host PC |
| Cable | USB-C to USB-A or USB-C to USB-C — must be a **data** cable, not charge-only |

**Power:** The USB-C port is both power input and OTG gadget port. If the host
supports USB-PD at 5V/3A, a single cable handles both. Otherwise power the board
separately via GPIO header pins 2/4 (5V) and 6 (GND) and use a data cable for USB.

The board exposes a composite USB device at boot:
- **NCM Ethernet** — `usb0` on board, `enxXXXXXXXXXXXX` on host
- **ACM serial console** — `/dev/ttyGS0` on board, `/dev/ttyACM0` on host

MAC addresses are derived from the board serial number and are stable across reboots.

---

## Host-Side Setup

```bash
# Enable internet sharing from host to board (run once)
./scripts/manage-net-sharing.sh enable

# Check status
./scripts/manage-net-sharing.sh status

# List detected gadget interfaces
./scripts/manage-net-sharing.sh list
```

`enable` creates a NetworkManager `ipv4.method=shared` connection on the gadget
interface: assigns `10.42.0.1/24` to the host, starts dnsmasq for DHCP, enables
IP forwarding and NAT. The connection is persistent and auto-activates on plug-in.

---

## Connectivity Checklist

```
[ ] USB-C data cable connected
[ ] Board booted — serial console available on /dev/ttyACM0
[ ] Host sees gadget interface:         ip link show | grep enx
[ ] Host sharing enabled:               ./scripts/manage-net-sharing.sh enable
[ ] Host has 10.42.0.1/24:             ip addr show enxXXXXXXXXXXXX
[ ] Board has IP on usb0:              ip addr show usb0
[ ] ARP resolved (REACHABLE):          ip neigh show dev usb0
[ ] Ping host from board:              ping -c 3 10.42.0.1
[ ] Internet from board:               ping -c 3 8.8.8.8
[ ] SSH from host:                     ssh root@<board-ip>
```

---

## Scenario 1 — Host sharing fails to activate

**Symptom:**
```
Warning: There is another connection with the name 'usb-gadget-sharing'.
Error: Connection activation failed: No suitable device found for this
connection (device docker0 not available ...)
```

**Diagnosis — find stale NM connections:**
```bash
# On host:
nmcli -g UUID,NAME connection show | grep 'usb-gadget-sharing'
nmcli connection show usb-gadget-sharing
```

**Fix — delete stale connections and re-enable:**
```bash
nmcli -g UUID,NAME connection show | grep ':usb-gadget-sharing$' | \
    cut -d: -f1 | xargs -I{} sudo nmcli connection delete {}
./scripts/manage-net-sharing.sh enable
```

---

## Scenario 2 — Board gets no IP address

**Symptom:** `ip addr show usb0` shows only IPv6 link-local, no IPv4.

**Step 1 — verify host sharing is active:**
```bash
# On host:
ip addr show enxXXXXXXXXXXXX          # should show 10.42.0.1/24
nmcli connection show usb-gadget-sharing | grep -E "STATE|IP4"
./scripts/manage-net-sharing.sh status
```

**Step 2 — check NM DHCP state on board:**
```bash
# On board:
journalctl -u NetworkManager | grep -i "usb0\|dhcp"
nmcli connection show usb-gadget | grep -E "ipv4|IP4"
```

**Step 3 — trigger a new DHCP attempt:**
```bash
# On board:
nmcli connection down usb-gadget && nmcli connection up usb-gadget
```

**Step 4 — capture DHCP traffic on host to see what is exchanged:**
```bash
# On host:
sudo tcpdump -i enxXXXXXXXXXXXX port 67 or port 68 -vn
```

Expected healthy exchange:
```
DISCOVER → (board to host, broadcast)
         ← OFFER    (host to board)
REQUEST  → (board to host)
         ← ACK      (host to board)
```

If you see repeated DISCOVERs with no REQUEST following, the board is not
receiving the OFFERs — diagnose RX (see Scenario 3).

---

## Scenario 3 — IP assigned but no connectivity (host→board packets lost)

**Symptom:** `usb0` has an IP but `ping 10.42.0.1` gets 100% packet loss.

**Step 1 — check RX/TX counters:**
```bash
# On board:
ip -s link show usb0
```
If TX is in the hundreds but RX is near zero, the USB receive path is broken.

**Step 2 — check ARP resolution:**
```bash
# On board:
ip neigh show dev usb0        # INCOMPLETE = ARP not resolved

# On host — watch ARP traffic:
sudo tcpdump -i enxXXXXXXXXXXXX arp -n
```
If the host sends ARP Replies but the board still shows INCOMPLETE, packets
are being lost between the USB layer and the network stack.

**Step 3 — check host TX errors:**
```bash
# On host:
ip -s link show enxXXXXXXXXXXXX    # look for TX errors > 0
```

**Step 4 — check packet filters on board:**
```bash
# On board:
nft list ruleset
iptables -L -n -v
sysctl net.ipv4.conf.usb0.rp_filter
```

**Step 5 — inspect USB gadget OUT endpoint state (DWC2 debugfs):**
```bash
# On board:
cat /sys/kernel/debug/usb/udc/1000480000.usb/ep1out
```

Healthy output: `NAKSts=0`, requests completing with non-zero `total_data`.

Broken output — DMA stall:
```
NAKSts=1                          # hardware NAKing all OUT tokens from host
DOEPDMA=0xXXXXXXXX               # DMA address programmed but never completing
req: ... EINPROGRESS total_data=0 # all requests stuck
```

**Step 6 — check DWC2 driver parameters:**
```bash
# On board:
dmesg | grep dwc2
```

Look for:
```
dwc2 1000480000.usb: supply vusb_d not found, using dummy regulator
dwc2 1000480000.usb: supply vusb_a not found, using dummy regulator
```
These indicate the USB PHY regulators are not defined in the device tree
(expected on RPi5 — cosmetic only).

**Step 7 — check for double enumeration:**
```bash
# On board:
dmesg | grep -iE "usb|dwc|ncm|udc" | tail -40
```
`new device is high-speed` appearing more than once indicates double
enumeration, which can leave the OUT endpoint in a bad state from the start.

**Root cause identified in kernel 6.6 on BCM2712:**
BCM2712 reports `arch=2` (DMA capable) but gadget DMA does not work in
peripheral mode — DMA completion interrupts never fire for OUT (host→board)
transfers. Fix: `g_dma=false` in `dwc2_set_bcm_params()` in
`drivers/usb/dwc2/params.c` (patch in `meta-rpi-extensions/`).

> **Warning:** Do not run `echo "" > /sys/kernel/config/usb_gadget/wendyos_device/UDC`
> on an affected board. It triggers `ep_stop_xfr` timeouts that leave Global OUT
> NAK permanently asserted, making RX completely dead. Reboot instead.

**Fallback — use WiFi while diagnosing:**
```bash
# On board (via serial console /dev/ttyACM0):
nmcli device wifi connect "YOUR_SSID" password "YOUR_PASSWORD"
ip addr show wlan0
```

---

## Reference — All Diagnostic Commands

### On the board
```bash
# USB gadget service
systemctl status gadget-setup
journalctl -u gadget-setup

# Network interface
ip addr show usb0
ip -s link show usb0
ip neigh show dev usb0
ip neigh flush dev usb0

# NetworkManager
systemctl status NetworkManager
journalctl -u NetworkManager | grep -i usb0
nmcli connection show usb-gadget
nmcli connection show usb-gadget | grep -E "ipv4|IP4"
nmcli connection down usb-gadget && nmcli connection up usb-gadget

# DWC2 USB controller (debugfs)
cat /sys/kernel/debug/usb/udc/1000480000.usb/ep1out
cat /sys/kernel/debug/usb/udc/1000480000.usb/ep1in
ls /sys/kernel/debug/usb/udc/1000480000.usb/
ls /sys/class/udc/

# Gadget configfs
ls /sys/kernel/config/usb_gadget/wendyos_device/functions/
cat /sys/kernel/config/usb_gadget/wendyos_device/UDC

# Kernel messages
dmesg | grep dwc2
dmesg | grep -iE "usb|dwc|ncm|udc" | tail -40

# Packet filters
nft list ruleset
iptables -L -n -v
sysctl net.ipv4.conf.usb0.rp_filter

# WiFi fallback
nmcli device wifi connect "YOUR_SSID" password "YOUR_PASSWORD"
ip addr show wlan0
```

### On the host
```bash
# Gadget interface detection
ip link show | grep enx
./scripts/manage-net-sharing.sh list
./scripts/manage-net-sharing.sh status
./scripts/manage-net-sharing.sh enable
./scripts/manage-net-sharing.sh disable

# Interface stats (watch for TX errors)
ip addr show enxXXXXXXXXXXXX
ip -s link show enxXXXXXXXXXXXX

# ARP
ip neigh show dev enxXXXXXXXXXXXX
sudo tcpdump -i enxXXXXXXXXXXXX arp -n

# DHCP
sudo tcpdump -i enxXXXXXXXXXXXX port 67 or port 68 -vn

# NM stale connections
nmcli -g UUID,NAME connection show | grep 'usb-gadget-sharing'
sudo nmcli connection delete usb-gadget-sharing

# Serial console
screen /dev/ttyACM0
picocom /dev/ttyACM0

# SSH
ssh root@<board-ip>
```
