# Connect to Wendy Net from your computer

Join Wendy Net to discover and manage devices that are reachable through other
mesh devices. The CLI runs a background router on your computer and uses the
organization selected in your sign-in profile. You choose the destination
device; the router automatically chooses nearby connections and paths.

Devices with Wendy Net disabled can still be discovered and managed directly
on your LAN. Use `wendy discover --type lan` for those devices; joining the mesh
is a separate operation.

## Join and discover

Sign in to your organization and connect this computer to the mesh:

```sh
wendy auth login
wendy device wendy-net connect
wendy discover --type mesh
wendy device wendy-net services
```

The CLI automatically finds and authenticates nearby WendyOS and supported
PSRAM Lite routers over LAN and Bluetooth. It retries suitable neighbours after
a connection failure. LAN connections use
TCP with TLS; macOS and Linux also support outgoing Bluetooth connections.
Direct Lite connections require firmware with native operator-router support.
A C5 with PSRAM can route to other devices; C6 remains a leaf and cannot be the
first router. The CLI can keep several connections and waits for a verified
route before reporting readiness. You do not need to supply router addresses
or Bluetooth identifiers.

Commands share one background router for the selected sign-in profile. After
`connect`, it stays running when commands exit and can forward traffic between
peers. Stop it when finished:

```sh
wendy device wendy-net disconnect
```

Disconnecting closes this computer's mesh connections and stops it forwarding
traffic. Your saved sign-in and mesh identity remain available for reconnecting.
Discovery and device commands leave the mesh disconnected until you explicitly
run `wendy device wendy-net connect` again. Ordinary LAN management remains
available while this computer is disconnected from the mesh.

A requested Nearby or mesh discovery can also start a temporary router using the
selected sign-in profile. Unless explicitly enabled with
`connect`, an automatic router can stop after five minutes without command
activity or transit traffic. Later discovery can start it again unless you explicitly disconnected. Reading local
router status or opening the Wendy Client Networks view only observes an
existing router.

## Backup connections and recovery

WendyOS and CLI routers can keep more than one connection to a neighbour. When
capacity allows, the router can switch to a faster connection and keep a slower
one, such as Bluetooth, ready as a backup. It keeps the working connection while
checking the new one. You do not need to select a router or connection manually.

Backups share the device's connection limits. A router may close an unused backup
to make room for another neighbour. Recovery starts after a failure is detected;
it is not an instant handover, and application connections can still be
interrupted. Check a real command to your destination after a network change.

Both ends need compatible versions to keep a backup connection. An older peer
may close Bluetooth while LAN works, then need time to establish a new Bluetooth
connection after LAN fails.

## Check connection status

Inspect the background router on this computer without starting it:

```sh
wendy --json device wendy-net topology --local-cli
```

To inspect a device's mesh settings and running connections, select that device:

```sh
wendy --device DEVICE_SELECTOR device wendy-net status
```

A ready Bluetooth connection can carry mesh traffic while LAN is still waiting
for a usable network interface. The status describes the available connections;
it does not guarantee that every device is reachable. Verify the destination
with `wendy --device DEVICE_SELECTOR device info`. In the desktop Networks view,
Cloud display names help identify devices; use their returned selectors for
commands.

## Select a device or application service

Use the `mesh://TENANT_UUID/DEVICE_UUID` selector returned by discovery with
ordinary device commands:

```sh
wendy --device mesh://TENANT_UUID/DEVICE_UUID device info
wendy --device mesh://TENANT_UUID/DEVICE_UUID device wendy-net status
wendy --device mesh://TENANT_UUID/DEVICE_UUID device wendy-net topology
wendy device wendy-net services --type _http._tcp
```

Discovery lists devices and services only while they have a current, verified
route. A disconnected device may disappear from the list.
Mesh connections authenticate the destination independently of intermediate
routers. Lite devices support device management, app upload/start, logs and
supported SensorLink commands. Container registry, deployment and other
agent-only commands require WendyOS.

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
are ignored when loading existing files and removed when settings are saved. They do not enable a carrier; configure `tls_listen` explicitly for native TLS. Neighbours are
discovered automatically rather than entered in a separate leaf list.

## Version compatibility

Use matching operator-capable agents and clients. Upgrading the CLI stops and
replaces an older local background router when needed. Previously saved router
choices are no longer used: connections are discovered automatically. Previously enabled mesh participation stays enabled.

Older agents may reject saved mesh identity data with `invalid directory
receipt`. Downgrading after joining the mesh is not yet qualified. Do not delete
mesh identity files or re-enroll to force a downgrade: that would reset the
identity's protection against old messages being replayed.

For native C5/C6 firmware with runtime radio management, follow the
[Bluetooth radio command reference](/docs/advanced/clients/wendy-cli/commands/device/bluetooth#wendy-device-bluetooth-mode).

Start with normal mode and firmware/controller-default power. The reference
covers support on both peers, WendyOS coding-selection limits, status fields and
recovery. A Lite profile change can restart the device; keep a working management
path. Maximum power requires an explicit request.

For existing WendyOS Bluetooth links, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).

## Operator routers

An operator router uses your organization's operator certificate rather than an
enrolled device identity. Each CLI installation has its own persistent router
identity and address, so multiple computers can participate independently.
Connections authenticate both the first router and the destination against the
selected organization and operator credentials. The CLI also needs router startup
and discovery support; agent-side admission alone does not start it.

On supported WendyOS nodes, an incoming operator connection over Bluetooth uses
one separate slot, in addition to the device-neighbour slots. The node must trust
your organization's primary operator certificates. All connections still share
the adapter's bandwidth and hardware connection limits.

## Application streams

WendyOS agent clients can use `WendyMeshService.MeshDial` on an existing,
authenticated same-tenant agent connection. Canonical enrolled-device and primary
operator identities are admitted. The current running-app ingress registry
still authorizes the requested port; a discovery record alone does not grant
access.

The first client `MeshDialMessage` carries a `MeshDialOpen` port request in its
`open` field. The client waits for an empty `MeshDialData` ready frame before
sending application data. The server sends this frame only after the authorized
destination connection opens. Later client messages use the `data` field;
`MeshDialData` carries payload bytes and directional half-close. The application stream
owns one RPC stream; closing it leaves the shared agent connection available.
Application bytes remain inside that agent TLS connection, without another TLS
handshake around it.

Each server allows eight concurrent application streams. The client adapters
emit payload frames of at most 16 KiB. The client helper and its lifetime,
deadline and backpressure contract are in `go/internal/shared/meshappstream`.

## Topology service snapshots

For the agent's `GetLocalMeshTopology` RPC, set the request field
`include_services: true` to request public service descriptions from the same
verified manifests as the device announcements. Ordinary device polling can
leave this field unset to keep responses small.

Each announcement's optional `services` list contains `LocalMeshService` rows:
`mesh`, `app_id`, `service_id`, `type`, `instance`, `port` and `txt`. Each row
inherits its announcement's device identity, revision and expiry. Service rows
require an unexpired, non-withdrawn manifest and a matching authorized,
non-unreachable route. Gateway capabilities without an application socket are
omitted.

Check `runtime_available`, `services_included` and `truncated` when interpreting
the result. `services_included: true` confirms that the reporter handled the
opt-in request. If it is false after requesting services, reporting was not
handled or its snapshot was unavailable; do not treat that as an empty service
catalogue. The response bounds service rows to 1024 and their serialized-size
budget to 256 KiB. `truncated` can also indicate a limit in another topology
component, so missing rows in a truncated response are an incomplete observation.

Announcements also carry the public `agent_port`, `agent_transport`, `platform`
and `caps` hints. Authenticate the destination independently before using an
endpoint. The topology is one local snapshot: its hints and service rows do not
prove end-to-end reachability or replace current application-port admission.
