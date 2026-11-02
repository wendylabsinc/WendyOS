# Wendy Lite and Wendy Net radio protocols

The original endpoint-only Lite protocol and its `leafBLEPeers`, `leafListen`
and `leafTimeURL` host configuration have been retired. Those fields are rejected
by the current configuration loader; remove them when upgrading.

The replacement uses ordinary authenticated router streams and complete signed
v4 device manifests. Native Lite discovery, radio admission and listener support
are integrated with the matching firmware and WendyOS carrier runtime. This
protocol-layer change alone does not activate that runtime.

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

## Version compatibility

Use matching operator-capable agents and clients. An older agent can reject
persisted operator routing receipts with `invalid directory receipt`, even
when its kernel routing cleanup succeeds. Downgrading after operator mesh use
is therefore not yet qualified. Do not delete routing history or re-enroll as
a routine downgrade workaround: that does not preserve the original identity's
replay protection.

For existing WendyOS Bluetooth links, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).
