# Retained operator mesh context

`wendy device wendy-net connect` discovers a same-tenant WendyOS TLS peer on
the LAN; `--address HOST:43024 --router DEVICE_UUID` selects an explicit peer.
On macOS/Linux, `--transport ble --address PERIPHERAL --router DEVICE_UUID`
selects a WendyOS BLE first hop instead, with optional `--psm` (default 129).
PERIPHERAL is a CoreBluetooth UUID on macOS or LE address on Linux; it is a
connection hint, while DEVICE_UUID remains the exact authenticated router.
A cold `mesh://TENANT_UUID/DEVICE_UUID` selection can prepare a missing context
automatically. Both paths prepare the current profile's operator context. It prints a canonical
`mesh://TENANT_UUID/DEVICE_UUID` selector. Ordinary `device info`, RPC commands,
saved defaults and MCP selection consume that same selector without another
operator-to-router TLS handshake. `wendy device wendy-net disconnect` closes the
current operator context; its non-secret first-hop recipe remains so a later
mesh command can cold-start it.

The existing `__session-broker` helper owns the portable IPv6 endpoint, signed
route proof/renewal, packet driver and one adjacent HTTP/2 agent stream. A private
startup lock serializes concurrent callers before launching a helper. A second
principal-scoped lifetime lock precedes any mesh handshake. First-hop changes
require the old context to be disconnected; recipes and endpoint counters live
under the selected persistent `WENDY_CONFIG_DIR`/normal Wendy profile, with
private permissions. Synthetic test credentials alone use temporary directories.

The primary credential actually selected by the caller is fingerprinted in the
helper recipe. The exact canonical operator principal and tenant stay separate
from the device identity. The first hop verifies its exact enrolled principal,
primary chain and the existing profile SPKI pin; no alternate Lite credential,
legacy organization requirement, source-address identity or neighbour identity
substitution is added. Credential replacement drains the retained context before
joining with the newly selected credential. Logout/unpin invalidation removes
its published IPC state using the existing broker invalidation path.

The default user-idle timeout is five minutes, configurable through
`sessionBrokerIdleTimeout`. Open IPC connections hold operator user leases through quiet local work. The
legacy direct broker retains its separate parent-death-pipe lease; operator
contexts must not be pinned merely because their launching MCP process survives.
Packet traffic and proof renewal
are internal work and do not reset that timer. Idle shutdown retires IPC, cancels
the attachment and joins its drivers; the adjacent router withdraws its claim.
A racing new invocation can reuse the current owner or retry after its lock
becomes available. A lost operator carrier closes that transport generation but
retains private IPC and watcher leases. The supervisor reconnects with the same
installation endpoint instance, a fresh incarnation and a greater persisted
proof revision. Explicit recipes retry their selected peer; automatic recipes
may rediscover another WendyOS peer or its changed LAN address. Reconnection
reloads current selected credentials and profile pins. A changed credential
retires the owner so a caller can join with the newly selected credential.
Existing flows fail normally on carrier loss; they are never replayed silently.
New RPCs, streams and directory snapshots use the current generation. A changed
first hop never substitutes its identity for the command's selected target.

Explicit remote `mesh://tenant/device` selectors also resolve through this
same retained context. The authenticated adjacent agent's fresh topology RPC
must show the target's live authorized /128 route and matching, unexpired
verified announcement. Public port/transport/platform hints select the native
agent endpoint. Its own end-to-end TLS independently verifies the exact remote
URI, tenant chain and selected profile's SPKI pin; the adjacent identity is never
substituted. Remote traffic has the actual operator5680 source, without SNAT.
Remote failures leave adjacent RPC/attachment health intact. The local routing
header is stripped before forwarding.

The broker retains up to 16 remote clients (each has 256 KiB read and write buffers,
plus independent HTTP/2 windows). Active RPC leases cannot be evicted; idle
entries are replaced oldest first and their network transports also use the
normal broker idle timeout. A full busy cache reports resource exhaustion.
Reconnects re-read live endpoint hints and current profile pins. A later signed
port change creates a new endpoint client while current calls keep their lease.
Lite/WendyCom endpoints use the bounded native adapter described in
[operator_lite.md](operator_lite.md). Nearby reads a shared bounded authenticated
directory/watch and does not claim presence from Cloud inventory. Auxiliary TCP
streams share the eight-stream Lite admission budget and target a port on the
exact selected mesh device after fresh manifest/route checks. RegistryDialer
uses this stream so image transfers retain end-to-end registry TLS. General
production registry/deploy/browser workflows, physical BLE first-hop coverage,
automatic BLE selection, direct Lite first attachment and Windows runtime
coverage remain qualification or implementation work; see the campaign's CLI
completion plan.
Mesh selectors stay typed URIs to prevent an auxiliary LAN fallback.

macOS/Linux use private Unix sockets. Windows operator contexts use per-user
named pipes, real ACL/owner checks, file locking and detached process creation;
the legacy direct broker's inherited-parent-file path remains disabled there.
Windows CLI and positive-test executables cross-build; actual Windows runtime
qualification remains outstanding. The wire attachment remains binary CBOR/DER. Base64 in the
existing hidden helper's non-secret local process recipe is not a mesh frame.

Positive tests cover two real isolated Linux kernel routers, one operator userspace stack, primary ML-DSA operator identity, native remote-agent TLS with true5680 source, 64 KiB RPC responses concurrent with adjacent RPC and proof renewal, remote offline handling, bounded retained client leases, and concurrent callers, a real detached child with an isolated profile, primary P-256/ML-DSA-65 credentials, original
TLS identity in RPC, warm independent IPC clients, renewal, quiet client leases,
idle route withdrawal while the launcher remains alive, explicit disconnect, ordinary device-info/default/MCP
selection, automatic LAN cold preparation, ordinary carrier restart with the
same retained IPC client and endpoint instance, handover directory source/target
separation, registry mTLS/64 KiB transfer, bounded auxiliary stream leases, and
unchanged legacy unary/streaming broker behavior. The adjacent
transport's 32KiB credit window and slow-consumer packet/renewal tests remain in
`shared/meshoperatorlink`. No lab hardware was deployed for this checkpoint.

Each production helper currently accepts one Spec and owns one userspace
endpoint. Different operator contexts start separate instances of this existing
helper, as direct device sessions already do. The gap report's eventual single
process owning several contexts is not implemented or qualified here. A Linux
race fixture colocating both the broker's endpoint and a second userspace router
endpoint stalled RPC admission; separate broker-process and idle-kernel-router
fixtures passed. That multi-endpoint interaction remains unresolved and needs
explicit simultaneous RPC/packet/renewal coverage before adding a shared process
owner. It is not an ESP capability restriction or evidence that multiple
operator contexts cannot be supported.

## Application services and forwarding

`wendy device wendy-net services --mesh lab --type _http._tcp` reads service
entries from the authenticated reporter's current combined manifests. Use JSON
to retain exact owner selectors, namespace/app/service identity, port, TXT and
the inherited manifest revision/expiry. There is no separate service gossip,
cache or refresh timer in the broker. Only an explicit browse requests service
contents; ordinary Nearby topology reads stay small. Both the source projection
and private IPC result have a 256 KiB service budget. The broker returns at most
512 rows and reports truncation so the query can be narrowed. An older reporter
which does not implement the opt-in projection reports unsupported, rather than
pretending to have an empty service inventory. Expired/unroutable owners disappear.

`wendy --device mesh://TENANT/DEVICE device wendy-net forward 8080:8000` binds a
loopback TCP listener and opens an authorized running-app stream per accepted
connection. Its existing operator-to-device agent TLS is reused end to end;
app traffic does not get another nested TLS layer. The running-app ingress
registry still owns port admission and revocation. Both listener and destination
bound simultaneous streams to eight, retain backpressure, drain half-closes and
release owned flows on cancellation. The ordinary registry dialer remains an
opaque raw TCP connection with its own end-to-end registry mTLS, because the
registry is a distinct TLS protocol endpoint. UDP forwarding and native Lite app
forwarding remain separate implementation/qualification work.

The private installation-state upgrade preserves operator endpoint instance and
counter when loading the earlier local CBOR spelling. This does not accept an
old mesh wire format. Router-side expired operator receipt cleanup must happen
before consulting its live revision floor; a stale elapsed receipt must not
permanently exclude a fresh returning installation. Current positive expiry
and P-256/ML-DSA-65 reconnect tests cover this ordinary lease lifecycle.

BLE first attachment uses the same primary operator certificate, signed route
proof, portable endpoint, management stream and retained reconnect machinery as
TCP. The server selects the distinct operator ALPN before reserving a neighbour
slot and has one separately bounded BLE operator attachment. Device slots and
candidate selection remain unchanged. Operators share the physical radio and
handshake gate. They do not inherit device session tickets. Positive host tests
exercise both primary signature algorithms with all router slots occupied;
physical-radio qualification remains outstanding. Received management bytes are
drained before an ordered remote close; local Close still retires its buffer.

Operator mesh management connections carry the exact typed target separately
from Cloud's reconnect marker. App readiness opens an admitted application
stream on that authenticated connection, without probing a reported LAN IP or
requiring an OS route to the userspace mesh address. HTTP readiness completes a
HEAD request rather than leaving an empty app connection ahead of a browser.

An attached HTTP app's openURL hook gets an ephemeral loopback forward through
that same application stream. The URL retains its path, query and fragment;
the manifest/hook description is not mutated. Up to eight viewers may be active
in this CLI process, each subject to the existing flow and server admission
bounds. The run context and selected connection both own their viewers; closing
either joins the listener and flows. Detached commands do not leave a browser
proxy process behind; use explicit `device wendy-net forward` for a persistent
viewer. Arbitrary host-side programs still need a local forward instead of an
OS route to the userspace mesh address.
