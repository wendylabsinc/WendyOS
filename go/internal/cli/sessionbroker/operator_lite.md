# Lite management through an operator attachment

A `mesh://<tenant UUID>/<device UUID>` target is described by the existing
operator broker. The broker's own routing node must hold a fresh verified
manifest and a live authorized route with the exact same principal,
IPv6 prefix, origin router, sequence and signed expiry. `platform=Lite` plus
`agentTransport=mtls-v1` selects native WendyCom at the signed `agentPort`.
WendyOS targets retain their gRPC adapter. No Cloud inventory, LAN address, or
port guess substitutes for a missing route or signed endpoint.

The private local IPC methods are `/wendy.sessionbroker.ManagementV1/Describe`
and `/wendy.sessionbroker.ManagementV1/LiteTCP`. Their target uses the existing
`wendy-operator-target` metadata field. Describe returns JSON `OperatorTargetInfo`.
LiteTCP takes an empty open message, checks the current target again, dials the
signed IPv6 TCP endpoint through the broker's existing `Session.DialContext`,
and returns an empty ready message. Subsequent messages are byte frames of at
most 16 KiB. No gRPC request reaches the Lite device.

There are at most eight admitted streams shared by Lite management and auxiliary
TCP connections. Each direction has one reader,
one serialized writer and a 16 KiB copy; no application output queue grows
behind a slow consumer. gRPC's bounded HTTP/2 flow control applies backpressure
to the socket and then the mesh endpoint. The Lite client additionally sets
16 KiB send/receive message limits. Existing generic gRPC broker message limits
remain unchanged. An open has a 15-second routed TCP budget and observes caller
cancellation. Once opened, the returned socket has normal dial semantics:
setup-context cancellation does not close it; its owner must close it. Close
cancels both directions. CloseWrite sends request EOF after accepted bytes;
a TCP peer can finish its response. A live stream holds the existing broker
activity lease, and attachment shutdown cancels its stream context.

The normal Lite client establishes exactly one destination TLS session over
those bytes and then runs WendyCom. It authenticates the exact selected device
UUID and tenant, validates the normal issuer chain (including the existing
Lite Device CA fallback), and reloads/checks profile device pins after successful
verification. Its client credential comes only from the selected primary
operator context; a cached/renewed Lite credential must have that exact primary
operator principal. The broker does not terminate destination TLS. There is no
app-session TLS wrapper and no second operator attachment.

The provider's existing device info, app upload/start, console, network config,
and SensorLink paths use this same client. Signed `caps` are retained as
selection metadata, including unknown optional values; `caps=sensors` describes
the native WendyCom subfeature, not a second mesh service or a permission grant.
Actual sensor definitions still come from the authenticated SensorLink manifest.
Agent-only registry/deploy/MCP commands continue to report that they require a
WendyOS agent. USB installation/enrollment remains physical. The first hop may be
WendyOS or a PSRAM Lite router with native operator-router support (wendy-lite
PR #121). Bootstrap waits for its signed route and does not probe a gRPC agent.
C6 remains a leaf. Mesh Nearby uses the retained broker directory and route-filtered
signed capabilities.

The local broker supports Unix sockets on macOS/Linux and per-user named pipes
on Windows. Host BLE support is available on macOS and Linux; Windows TCP code
builds, but physical Windows device interoperability is not established.
For user commands, see the
[CLI mesh guide](../assets/docs/development/local-mesh-radio.md).
