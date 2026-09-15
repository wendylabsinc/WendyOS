# WiFi Configuration

WendyOS manages WiFi through NetworkManager. There are three ways to configure WiFi: pre-staging credentials on the config partition before first boot, using the `wendy device wifi` CLI from a connected host, or running `nmcli` directly on the device.

## How WiFi connections are stored

NetworkManager stores connection profiles as `.nmconnection` files. WendyOS splits these into two locations:

| Location | Who writes it | Persistence |
|----------|---------------|-------------|
| `/usr/lib/NetworkManager/system-connections/` | OTA image (distro-managed) | Replaced on OTA update |
| `/etc/NetworkManager/system-connections/` | User / `wendy-agent` | Bind-mounted from `/data/etc/NetworkManager/system-connections/`; survives OTA updates |

`wendyos-etc-binds.service` establishes the bind mount early in boot so that NetworkManager always sees user-added connections at `/etc`. A hash-based migration step removes stale distro-managed copies from `/data` while preserving connections the user has modified.

## Pre-staging credentials via `wendy.conf`

Place a `wendy.conf` file on the `config` partition before first boot. The agent reads it on startup, applies the WiFi configuration, then **deletes the file**:

```ini
[wifi]
ssid = MyNetwork
password = hunter2
```

The agent calls `nmcli device wifi connect <ssid> password <password>`, which creates a persistent NetworkManager profile in `/etc/NetworkManager/system-connections/`. Omit `password` for open networks.

`wendy install` writes this file automatically when you supply `--wifi-ssid` and `--wifi-password` flags or respond to the interactive prompt.

See [Config Partition](../config-partition.md) for the full `wendy.conf` specification.

## Using the `wendy device wifi` CLI

The `wendy` CLI (running on a host machine) communicates with `wendy-agent` over the network or Bluetooth to manage WiFi remotely.

### Interactive TUI

Running `wendy device wifi` opens a full-screen table listing nearby networks:

```sh
wendy device wifi
```

### Subcommands

```sh
# List visible networks (shows SSID, security, signal, known/connected status)
wendy device wifi list

# List in JSON format (includes priority and RSSI)
wendy device wifi list --json

# Connect to a network (prompts for SSID and password interactively)
wendy device wifi connect

# Connect non-interactively
wendy device wifi connect --ssid "MyNetwork" --password "hunter2"

# Connect to a hidden network
wendy device wifi connect --ssid "HiddenNet" --password "secret"

# Show current connection status
wendy device wifi status

# Disconnect from the current network
wendy device wifi disconnect

# Remove a saved network
wendy device wifi forget --ssid "OldNetwork"

# Set autoconnect priority of a known network
wendy device wifi rank --ssid "Home" --priority 10

# Bulk-reorder known networks (highest priority first)
wendy device wifi rank --order "Home,Office,Cafe"
```

## Using `nmcli` directly on the device

SSH into the device and run `nmcli`:

```sh
# Scan for available networks
nmcli device wifi list

# Connect (creates a persistent profile)
nmcli device wifi connect "SSID" password "password"

# Show all saved connections
nmcli connection show

# Disconnect from the current WiFi
nmcli device disconnect wlan0

# Delete a saved profile
nmcli connection delete "SSID"

# Show current connection status
nmcli device status
```

## NetworkManager configuration

The main NM configuration is at `/etc/NetworkManager/NetworkManager.conf`:

```ini
[main]
dns=default
rc-manager=file
plugins=keyfile
no-auto-default=lo
connectivity.enabled=false

[logging]
level=INFO
backend=journal

[device]
wifi.scan-rand-mac-address=no
```

Key decisions:
- By default, DNS is managed by NM writing `/etc/resolv.conf` directly (`rc-manager=file`). Optionally, `systemd-resolved` can be enabled instead by adding `resolved` to `DISTRO_FEATURES` at build time. When its `127.0.0.53:53` stub is active, newly deployed host-network containers use it so upstream DNS changes propagate without a container restart; their managed resolver file also copies the currently known search domains. Otherwise, the agent retains an inode-pinning host-file fallback for compatibility.
- Random MAC address scanning is disabled so device identity remains stable during scans.
- `connectivity.enabled=false` saves bandwidth by not sending periodic HTTP connectivity probes.

## USB gadget and WiFi coexistence

The USB gadget interface (`usb0`) is configured with route metric 700, higher than typical WiFi/Ethernet auto-metrics (~100–600). When WiFi is connected, it becomes the preferred default route. The USB gadget remains available as a fallback and for direct host-to-device access.

The `WENDYOS_USB_NET_MODE` build variable controls the IPv4 method for `usb0`:

| Value | Behaviour |
|-------|-----------|
| `link-local` (default) | RFC 3927 self-assigned 169.254.x.x address; discovery via mDNS/Avahi |
| `dhcp-client` | Device requests an address from the host's DHCP server |
| `dhcp-server` | Device runs a DHCP server (requires `dnsmasq`; NM shared mode) |

## mDNS discovery

WendyOS advertises itself via Avahi using the service type `_wendyos._udp` on port 50051. The `WENDYOS_MDNS_INTERFACES` variable controls which interfaces Avahi uses:

- Default (`""`): all interfaces (both `usb0` and `wlan0`)
- Set to `"usb0"` to restrict discovery to the USB gadget only

On a host with Avahi or mDNS, find devices with:

```sh
dns-sd -B _wendyos._udp local.
```
