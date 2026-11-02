# Connect to Wendy Net from your computer

The Wendy CLI can join a mesh as a background router, discover devices and
services, and connect to them without a shared physical LAN. It uses the active
operator profile and the same tenant authentication as ordinary device commands.
Its routing identity belongs to this CLI installation; it is not an enrolled
device.

## Operator routers

The operator admission layer lets a CLI installation participate under its
organization's operator certificate instead of enrolling the computer as a
device. Each installation has a persistent, independent router identity and
address. Multiple computers or operator profiles can therefore participate
without sharing routing state or changing an enrolled device's identity.
The carrier and destination still authenticate the selected tenant and exact
operator credentials. Use the explicit connect and disconnect commands below to control
participation.

On supported WendyOS nodes, incoming operator connections over bluetooth use
one separate admission slot rather than a device-neighbor slot. This requires
the node's primary operator trust anchors and still shares the physical
adapter's bandwidth and connection limits.

## Join and discover

Sign in and enable a persistent background router:

```sh
wendy auth login
wendy device wendy-net connect
wendy discover --type mesh
wendy device wendy-net services
```

The CLI finds available WendyOS neighbours on LAN and BLE and retries suitable
candidates when a connection fails. LAN connections use TCP with TLS; supported
macOS and Linux hosts can also make outgoing BLE connections. The CLI's first
neighbour must currently be WendyOS. Lite devices can be reached through that
router, but cannot be the CLI's direct first hop.

Commands share one background router for the selected operator profile. After
`connect`, it stays running when commands exit and can forward traffic between
peers. Stop it when finished:

```sh
wendy device wendy-net disconnect
```

Disconnecting ends the current mesh connections and forwarding while preserving
the installation's routing identity.

A requested Nearby or mesh discovery can also start a temporary router using the
selected profile's cached operator certificate. Unless explicitly enabled with
`connect`, an automatic router can stop after five minutes without command
activity or transit traffic. Later discovery can start it again. Reading local
router status or opening the Wendy Client Networks view only observes an
existing router.

## Select a device or application service

Use the `mesh://TENANT_UUID/DEVICE_UUID` selector returned by discovery with
ordinary device commands:

```sh
wendy --device mesh://TENANT_UUID/DEVICE_UUID device info
wendy --device mesh://TENANT_UUID/DEVICE_UUID device wendy-net status
wendy --device mesh://TENANT_UUID/DEVICE_UUID device wendy-net topology
wendy device wendy-net services --type _http._tcp
```

Discovery and service browsing require a current signed device manifest and a
usable route. Disconnected or expired owners disappear from the available list.
Mesh connections authenticate the destination independently of intermediate
routers. Lite management, app upload/start, logs and supported SensorLink commands
use WendyCom. Container registry, deployment and other agent-only commands still
require WendyOS.

For Lite topology, use matching CLI and firmware versions with WendyCom
topology support.

To use a Lite guest's advertised TCP service from a local program, forward a
local port to its published port:

```sh
wendy --device mesh://TENANT_UUID/DEVICE_UUID device wendy-net forward 8080:80
```

The target must currently advertise port 80 as a TCP service. The command binds
`127.0.0.1:8080` and runs until Ctrl-C. The application may still require its own
authorization. UDP forwarding is not supported. Devices with small connection
budgets can reject additional streams; close unused connections and retry.

## Select an explicit first neighbour

Automatic discovery is normally enough. To connect to a known WendyOS router,
supply its enrolled device UUID and TLS carrier address together:

```sh
wendy device wendy-net connect --transport tcp \
  --address ROUTER_IP:TLS_CARRIER_PORT --router ROUTER_DEVICE_UUID
```

For Bluetooth, use `--transport ble` with the host's discovered peripheral identifier
or Bluetooth address. The selected identity is authenticated before use; an
address alone does not establish trust. Slow Bluetooth setup or several mesh hops can
make cold connections take longer than ordinary LAN connections.

## Configure a Lite device

Wendy Net is off by default on Lite. Select its existing management connection
and enable supported carriers, for example over USB:

```sh
wendy --device wendy-lite:PORT device wendy-net configure \
  --participate=true --ble=true --infrastructure-wifi=true --nan=false
wendy --device wendy-lite:PORT device wendy-net status
```

C5 devices with PSRAM can route across several peers. The constrained C6 profile
allows only one Bluetooth or Wi-Fi carrier; enabling both returns an error. Select
one explicitly, for example `--ble=false --infrastructure-wifi=true` for Wi-Fi.
Changing C6 between those carriers saves settings and restarts it; the CLI
reports that restart before the connection closes. Unsupported carriers return
an explicit error. NAN and Internet sharing are not available in these Lite
profiles. C6's constrained profile also disables Cloud connectivity.

Omitted options preserve existing settings. Disable participation with
`--participate=false`; mesh tasks and their allocations are released. For
Bluetooth configuration on WendyOS, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).

## Upgrade from the old Lite protocol

Upgrade participating firmware together. Lite uses the same complete signed
binary manifests and routing protocol as WendyOS; the old special leaf protocol
is removed. The former `leafBLEPeers`, `leafListen` and `leafTimeURL` settings
are rejected by the configuration loader and must be removed. Neighbours are
discovered automatically rather than entered in a separate leaf list.

## Version compatibility

Use matching operator-capable agents and clients. An older agent can reject
persisted operator routing receipts with `invalid directory receipt`, even
when its kernel routing cleanup succeeds. Downgrading after operator mesh use
is therefore not yet qualified. Do not delete routing history or re-enroll as
a routine downgrade workaround: that does not preserve the original identity's
replay protection.
