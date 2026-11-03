# mDNS / Avahi Service Discovery

WendyOS uses Avahi to broadcast its presence over mDNS (Multicast DNS, RFC 6762) so that the wendy-agent on the host can discover the device without knowing its IP address in advance. The device announces a `_wendyos._udp` service that carries the device UUID, name, and display name as DNS-SD TXT records.

## Service Advertisement

The Avahi service definition is installed at `/etc/avahi/services/wendyos-mdns.service`:

```xml
<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="yes">%h</name>
  <service protocol="any">
    <type>_wendyos._udp</type>
    <port>50051</port>
    <txt-record>id=WENDY_DEVICE_ID</txt-record>
    <txt-record>name=WENDY_DEVICE_NAME</txt-record>
    <txt-record>displayname=WENDY_DISPLAY_NAME</txt-record>
  </service>
</service-group>
```

The placeholder tokens (`WENDY_DEVICE_ID` etc.) are replaced during boot. On
later boots, the same updater replaces the current TXT record values again; it
is not limited to the original placeholders (see below).

- **Service type**: `_wendyos._udp`
- **Port**: `50051` pre-provisioning; updated to `50052` (mTLS) automatically once the device enrolls
- **Instance name**: The device hostname (`%h` expands to the current hostname as set by Avahi)
- **TXT records**: `id`, `name`, `displayname`, plus provisioning records such as `tls`, `assetid`, and `orgid` when applicable

Provisioning updates the port and may add `tls`, `assetid`, and `orgid`. Some
service templates also contain an optional `fqdn` record. The agent preserves
these records when it updates the hostname-derived TXT values.

The agent also keeps a `wired` record listing the routable IPv4 addresses of
its connected Ethernet ports, other than the address it takes on a camera port.
It updates the record once a new list has held for two checks 10 seconds apart,
or for about a minute when the new list drops an advertised address, and
restarts Avahi only when the record changes.

## Boot-time mDNS Identity Update

On every boot, `wendyos-identity.service` runs `update-mdns-uuid.sh` from
`recipes-core/wendyos-identity/`. The script reads
`/etc/wendyos/device-uuid` and `/etc/wendyos/device-name`, derives the title-case
display name, and replaces the current `id`, `name`, and `displayname` values in
the Avahi service file. Matching records by key makes this update idempotent; it
is not a one-time placeholder replacement. The equivalent data flow is:

```bash
UUID=$(cat /etc/wendyos/device-uuid)
DEVICE_NAME=$(cat /etc/wendyos/device-name 2>/dev/null || echo "unknown-device")
DISPLAY_NAME=$(echo "$DEVICE_NAME" | sed 's/-/ /g' | \
    awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) tolower(substr($i,2));}1')

# Replace the value of each matching <txt-record>key=...</txt-record> entry.
set_txt id "$UUID"
set_txt name "$DEVICE_NAME"
set_txt displayname "$DISPLAY_NAME"
```

The `displayname` field converts the hyphen-separated device name to Title Case (e.g. `my-device` becomes `My Device`).

If UUID or device-name files are not yet present, the script retries for up to
10 seconds to handle ordering races. After updating the file it calls
`avahi-daemon --reload` if the daemon is already running.

Enrollment also updates identity before the next boot: when loading
pre-provisioned credentials or completing live re-enrollment, the agent writes
the certificate's device UUID to `/etc/wendyos/device-uuid` and the service's
`id=` TXT record before restarting Avahi. A legacy asset certificate carries
no device UUID and leaves these records unchanged. See
[Pre-provisioning](../config-partition.md#pre-provisioning-provisioningjson)
for hostname preservation.

`wendy device rename` persists the operator-selected hostname in
`/etc/wendy-agent/hostname` and updates the Avahi records at rename time; it
does not overwrite the generated `/etc/wendyos/device-name`. Consequently, the
boot-time identity update above can replace `name` and `displayname` with the
generated device name. At agent startup, after `wendyos-identity.service` and
`avahi-daemon.service`, `ReassertHostnameAdvertisement` compares the service
file with the persisted explicit hostname and restores the hostname-derived TXT
records when needed. It restarts Avahi only when it changes the service file.
The same re-assertion repairs a fresh service file after an A/B OTA slot switch.

## Hostname Generation

Avahi broadcasts the device hostname as `<hostname>.local`. The hostname is set by `generate-hostname.sh` via `wendyos-hostname.service`, which runs before `avahi-daemon.service`:

**Resolution order:**
1. `/etc/wendy-agent/hostname` — literal hostname set via `wendy device rename` (no prefix)
2. `/etc/wendyos/device-name` — use as-is, lowercased, prefixed with `wendyos-`
   → `wendyos-<device-name>.local`
3. `/etc/wendyos/device-uuid` — take the last 8 hex characters (without dashes)
   → `wendyos-<8-char-uuid-suffix>.local`
4. Legacy fallbacks: RPi serial from `/proc/cpuinfo`, first 16 chars of `/etc/machine-id`, first MAC address, random hex

The hostname is written to `/etc/hostname` using a direct write (not `hostnamectl`, to avoid EBUSY issues with bind mounts) and also added to `/etc/hosts` as `127.0.1.1 <hostname> <hostname>.local`.

> **Rename persistence:** The hostname itself remains stable across reboots and
> OTA updates because `/etc/wendy-agent/hostname` is backed by `/data` and has
> highest precedence above. The Avahi `name` and `displayname` TXT records are
> separate rootfs state and can be re-derived from `/etc/wendyos/device-name`
> during boot. The agent re-asserts those records from the persistent explicit
> hostname at startup and does nothing on devices that have never been renamed
> or whose records are already current.

A hostname override can be applied by creating `/etc/wendyos-hostname-override`.

## Avahi Configuration

The `avahi_%.bbappend` recipe configures `/etc/avahi/avahi-daemon.conf` at build time:

```
enable-dbus=yes
enable-reflector=no    # reflector wedges startup in avahi 0.8 (bug WDY-755)
use-ipv4=yes
use-ipv6=yes
publish-addresses=yes
publish-hinfo=yes
publish-workstation=yes
publish-domain=yes
```

The reflector is explicitly disabled. A bug in avahi 0.8 causes the daemon to hang during startup when the reflector is enabled, regardless of the number of interfaces — services never get published.

NSS is configured to resolve `.local` names via mDNS by appending to `nsswitch.conf`:
```
hosts: files mdns4_minimal [NOTFOUND=return] mdns4 dns
```

## Host-Side Discovery

### Linux (avahi-browse)

```bash
# One-time scan
avahi-browse -t -p _wendyos._udp

# Continuous with full resolution
avahi-browse -r _wendyos._udp

# Example output:
# =;enx...;IPv4;wendyos-my-device;_wendyos._udp;local;wendyos-my-device.local;10.42.0.2;50051;"id=xxxxxxxx-..."
```

### macOS (dns-sd)

```bash
# Browse
dns-sd -B _wendyos._udp local.

# Resolve an instance
dns-sd -L "wendyos-my-device" _wendyos._udp local.
```

### wendy-agent Discovery Code

The wendy-agent Go code (`internal/shared/discovery/`) uses `_wendyos._udp` as the service type constant. On Linux it browses through the Avahi daemon's D-Bus API (`org.freedesktop.Avahi` on the system bus) — no `avahi-browse` child process is spawned; it falls back to the `hashicorp/mdns` library, which queries each multicast-capable interface individually, only when the system bus or the Avahi service itself cannot be reached. On macOS it browses and resolves in-process through `<dns_sd.h>`, the same mDNSResponder daemon that the `dns-sd` tool wraps, so no helper processes are spawned.

**CLI-side note:** The shipped CLI binary is built with `CGO_ENABLED=0`, so it cannot use nss-mdns to resolve `.local` names. Instead, it performs its own mDNS browse for `.local` hostnames when connecting. Set `WENDY_MDNS_DEBUG=1` to log browse failures, or `WENDY_MDNS_TIMEOUT` (1s–30s) to adjust the timeout.

Both the primary Avahi D-Bus path and the `hashicorp/mdns` fallback path parse all TXT records into a key→value map, including the `tls` record.

TXT records are **unauthenticated**: they are advertised by whatever host answers on the network, and anyone who can reach the multicast segment can advertise `_wendyos._udp` with any TXT values it likes. They are used for **routing and display only**, never for trust. A device that advertises `tls=true` has `IsMTLS` set to `true`, which tells the CLI to dial the mTLS port (50052) instead of the plaintext one — that arithmetic is real and still applies — but it is not what decides whether the CLI trusts the host it ends up talking to. Trust is established afterward, from the certificate that host presents during the TLS handshake, checked against the identity pin recorded for the name being dialed (see the "Device identity pinning" section of `pki/README.md`). A spoofed TXT record can get a connection attempt redirected to the wrong port on the wrong host; it cannot get that host trusted — the certificate check and pin still have to pass. The fallback path also resolves `wendyosdevice` and `displayname` TXT records, matching the behaviour of the primary path.

IPv6 link-local addresses returned by mDNS are annotated with the zone ID (`%<ifname>`) so that the caller can use them directly in `net.Dial()` calls.

The `MDNSService` struct captures:
```go
type MDNSService struct {
    InstanceName string
    Hostname     string
    IPAddress    string        // link-local IPv6 includes %zone suffix
    Port         int
    TXTRecords   map[string]string   // all advertised TXT records
}
```

## TXT Record Reference

| Key | Value | Example |
|---|---|---|
| `id` | Device UUID (from `/etc/wendyos/device-uuid`; once enrolled, the UUID in the device certificate) | `550e8400-e29b-41d4-a716-446655440000` |
| `name` | Slug device name (from `/etc/wendyos/device-name`) | `my-device` |
| `displayname` | Title-cased device name | `My Device` |
| `fqdn` | Optional reverse-DNS name derived from the hostname when present in the service template | `sh.wendy.my-device` |
| `wendyosdevice` | Device UUID (preferred over `id` when present) | `769dc651-4eb2-49f3-b9f6-3e473f15694a` |
| `tls` | `"true"` when the device is provisioned and requires mTLS (port 50052) | `true` |
| `wired` | Comma-separated IPv4 addresses of the device's connected Ethernet ports (at most 8); absent when none. Used only to choose a route, never to trust a device | `192.168.2.50` |


## Connect Wendy Net devices over Ethernet or Wi-Fi

Devices enrolled in the same organization can find one another on a local
Ethernet or ordinary Wi-Fi network. Run this on each device you want to connect:

```sh
wendy net configure --participate=true --ethernet=true --infrastructure-wifi=true
wendy net status
wendy net topology
```

Add `--device <device>` to select a particular device. Check topology for
connected peers; saving settings alone does not confirm connectivity. Omitted
flags preserve saved choices. Set a flag to `false` to disable that connection
type. Changing saved settings restarts local routing and can interrupt traffic.

Ethernet is preferred over ordinary Wi-Fi, followed by the radio connections.
If a connection disappears, another enabled connection can take over. Existing
TCP sessions may survive a short outage; new connections wait for a valid route.

### Network requirements and privacy

Allow local multicast discovery on UDP port 5353 and encrypted peer connections
on UDP port 43023. Devices need usable IPv4 addresses; isolated Wi-Fi clients and
multicast filtering can prevent discovery. Container bridges, access-point
interfaces, and IPv4 link-local addresses are not used.

Discovery advertises device and organization identifiers in cleartext on the
local network. Enable it only where that disclosure is acceptable. A discovered
address is a hint: each peer must still authenticate before carrying traffic.

The discovery service is `_wendy-net._udp`; peer traffic uses QUIC over UDP.
There is no TCP fallback on these LAN connections.
