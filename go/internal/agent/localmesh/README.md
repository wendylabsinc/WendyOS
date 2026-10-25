# Wendy net peer transport and routing

This stage adds a Linux router using enrolled Wendy device certificates,
authenticated neighbour links, a Babel route engine and Wendy-owned TUNs.
Device presence travels over each neighbour's existing control channel.
It does not open a catalogue connection to every remote device.

## Explicit links and activation

`local-mesh.json` in the agent configuration directory (`/etc/wendy-agent` by
default) enables the router. A missing file leaves it disabled. On Linux, the
file and its directory must belong to the agent's effective user (root for the
system service), and neither may be writable by group or others. The file must
be regular; symlinks and special files are refused. Reads are bounded to 64 KiB.

For a chosen test topology, configure both ends of each edge:

```json
{
  "listen": "0.0.0.0:43020",
  "peers": [
    {"device": "2b1c0000-0000-4000-8000-000000000460", "address": "192.0.2.60:43020"}
  ]
}
```

Configured peers and discovered candidates are eligible regardless of UUID
ordering. The listener checks the configured peer list, where applicable, and
mutual TLS pins the full peer principal. The unauthenticated TCP preface is an
identity hint, never authorization. The framed TCP carrier simulates graph
topologies; automatically discovered LAN, NAN and BLE have their own providers.

Production Ethernet and infrastructure Wi-Fi discovery uses native QUIC over
IPv4 UDP port 43023, bound to each eligible physical interface and local address.
It never uses the simulator TCP adapter or falls back to TCP. LAN discovery
advertises `_wendy-net._udp` with `protocol=quic-v1`, full device principal and
mesh hash as opt-in cleartext hints; old `_wendy-mesh._tcp` advertisements are
not compatible. Permit UDP port 43023 for neighbour QUIC and UDP port 5353 for mDNS on the
selected LAN. IPv6 LAN discovery is not added here. Wi-Fi Aware/NAN already
uses native UDP QUIC and is unchanged.

Either full identity may initiate. Outbound mTLS pins the discovered complete
tenant/device principal. Inbound mTLS accepts an unknown enrolled same-tenant
device, derives its identity only from the verified certificate chain and then
checks selection, duplicate convergence and the identity-bound link hello.
Discovery, source IP and hello do not authenticate a peer. Every handshake,
including resumption, rechecks current trust and identity expiry. The unknown
LAN listener has a separate bounded server ticket domain. QUIC Retry verifies
source reachability; the shared 64-connection LAN policy bounds incoming
handshakes and live connections before TLS across all interfaces. Stalled handshakes retain a slot only until the
existing QUIC handshake timeout. No 0-RTT application data is accepted.

Each interface owns a single UDP socket/QUIC transport shared by all its peers.
Disconnecting or rejecting one peer closes only that QUIC connection. Interface
removal, cancellation or listener/mDNS failure closes the worker's endpoint,
pending handshakes and attached links before restart. Existing link costs,
control stream, carrier DATAGRAM framing, Babel/TUN and MTU remain unchanged.

Connection scheduling shares `ConnectionPolicy` and `CandidateQueue` across all
providers, with separate limits:

| Transport | Outgoing connections | Concurrent outgoing setup | Total incoming + outgoing |
| --- | ---: | ---: | ---: |
| BLE | 2 | 1 | 3 |
| Wi-Fi Aware | 2 | 2 | 4 |
| Ethernet + infrastructure Wi-Fi | 2 | 2 | 64 |

Ethernet and infrastructure Wi-Fi share their budget across interfaces. The
configured TCP test carrier uses the LAN limits in its own policy instance.
Pending setup reserves outgoing and total capacity, preventing concurrent
attempts from overshooting the established-connection limits. Incoming attempts
can use remaining total capacity after the outgoing target is reached. The node
also retains its existing 64-link global resource bound.

Candidate selection shuffles unpenalised candidates. Failed connections move
behind them for 30 seconds; penalised candidates are ordered oldest failure
first, so another failure moves a peer to the bottom again. Capacity or pacing
skips do not penalise a candidate. BLE retains its existing five-second outbound
start pacing, independently of its one-setup-at-a-time limit.

A progressing incoming attempt suppresses new outgoing setup to that peer once
its address or identity can be associated with a candidate. Stream transports
observe successful handshake reads/writes; NAN reports negotiation milestones.
Suppression expires after five idle seconds and ends on teardown. This is a
bounded hint, independent of certificate verification and existing handshake
deadlines. There is no UUID eligibility filter or isolated-node grace timer.

Crossed connections may still race. After certificate verification, mesh hello
and local node admission, both ends retain the same preferred connection:
lower UUID initiates for LAN/configured TCP/NAN, higher UUID for BLE. A sole
reverse connection remains usable; no existing connection is retired just
because a preferred attempt started. Only the losing stream/QUIC connection is
closed. BLE never disconnects the whole BlueZ device/ACL during this decision.
NAN tracks NDP incarnations independently and reference-counts shared underlay
neighbours, so one path cannot remove another path's neighbour entry. Ambiguous
firmware events do not select an arbitrary path for teardown.

A cheaper LAN link still replaces a slower link only to the same principal;
LAN does not consume radio capacity. The shared carrier preference can retain
NAN when shedding duplicate BLE links, independently of connection limits.
Only a fresh candidate for BLE can displace a redundant BLE link; NAN-only
presence cannot use the freed Bluetooth slot. Failed or expired candidates and
devices already reached on another carrier do not displace the fallback.

Before changing kernel networking state, the router checks actual IPv6 policy
routing and TUN APIs. Setup failures identify the failed operation and relevant
kernel requirements. Linux images need IPv6 policy tables, dummy interfaces and
TUN support; container UDP interception additionally needs TPROXY support. An
agent update cannot supply kernel features compiled out of an installed image.

## LAN and Wi-Fi Aware discovery

Both carriers advertise the same enrolled device principal and tenant-scoped
default mesh hint. The hint is the first eight bytes of SHA-256 over the exact
UTF-8 string `tenant:<canonical-tenant-uuid>:default`, without a newline. It is
an untrusted connection filter; QUIC mTLS still pins the complete principal.
Separate named-mesh certificate authorization is not implemented.

LAN discovery uses `_wendy-net._udp.local.` with TXT fields `protocol=quic-v1`,
`principal=<SPIFFE URI>`, `mesh=<16 lowercase hex characters>` and `version=1`.
SRV port, host and address records retain the native UDP contract. Versionless
advertisements still require the native UDP service and `protocol=quic-v1`;
the former TCP service is incompatible. Readers reject unsupported or malformed
explicit versions and duplicate keys, comparing keys without regard to case as
required by DNS-SD; unknown additional TXT keys are allowed. The additive
version field is compatible with native UDP readers that ignore it.

Synchronized Wi-Fi Aware discovery keeps service name `wendy.mesh.v1`. Its SSI
is binary, with all offsets below measured in bytes:

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 4 | ASCII magic `WNAN` |
| 4 | 1 | Binary payload version `0x01` |
| 5 | 1 | Flags: bit 0 means mesh hint present; all other bits must be zero |
| 6 | 16 | Tenant UUID, standard network-order UUID bytes |
| 22 | 16 | Device UUID, standard network-order UUID bytes |
| 38 | 8, optional | Raw mesh hint bytes, without hex encoding |

The payload is exactly 38 bytes without a mesh or 46 bytes with one. Both UUIDs
reconstruct `spiffe://wendy.sh/tenant/<tenant-uuid>/device/<device-uuid>`; the
URI template is fixed by this protocol version. Explicit nil UUIDs preserve
the existing identity contract. An all-zero hint is present if its flag is set;
it is not an absent mesh. Current enabled providers advertise the default mesh.
Mesh-less presence is representable but cannot initiate or accept a mesh path.

Publication, subscription, NDP requests, NDP responses and counter confirmations
all carry the same SSI. The supplicant control socket uses a hex rendering of
these bytes; that representation is decoded before transmission on the radio.
Discovery, incoming NDP requests and counter proposals must pass tenant and mesh
checks before affecting peer selection or creating/accepting a path. Unsupported
versions, unknown flags, invalid lengths and trailing bytes are rejected.

This binary v1 replaces the previous textual `wendy-nan:1:<device-uuid>`
advertisement. There is deliberately no legacy fallback: the old advertisement
lacks the tenant and mesh needed for early filtering. Update NAN peers together;
other carriers retain their existing wire compatibility. Discovery protocol
versions are independent of the authenticated mesh ALPN/hello version.

The full principal is now exposed to nearby NAN observers, as it already is on
the LAN through mDNS. This filter avoids ordinary cross-tenant connection loops;
it cannot prevent a sender from advertising a forged local-tenant claim. Only
certificate verification authorizes the link.

## Wi-Fi Aware recovery

Repeated failed data paths can trigger one retry of the router's private NAN
interface during an outage. A neighbor must remain authenticated for a full
minute before this private-interface retry becomes available for a later
outage. Discovery messages and transient handshakes cannot renew that budget.

The stronger radio-wide recovery still requires ownership of the shared NAN
interface, no application-owned NAN interface and the existing no-receive
evidence. It remains limited to one attempt per provider run. Recovery never
restarts the supplicant or station Wi-Fi.

## Lite leaf carrier

An opt-in `"leafListen": "0.0.0.0:43024"` in `local-mesh.json` enables a
separate listener for Wendy Lite endpoints. It can be enabled alone or alongside
the other carriers. `ConfigureLocalMesh` sets it through `leaf_listen`
(`wendy device wendy-net configure --leaf`, or `--leaf-listen <host:port>`);
an empty value turns it off. The leaf always initiates, regardless of UUID ordering.
The existing TCP/QUIC carrier retains its `WMT1` framing and negotiates the
router ALPN `wendy-net-router/2`. No downgrade or fallback between the carriers is attempted.

The leaf sends `WML3`, its 16-byte tenant UUID and its 16-byte device UUID.
This 36-byte preface is untrusted. TLS 1.3 then verifies both enrolled device
certificates and pins the complete expected principal, in the same tenant,
using ALPN `wendy-local-mesh-leaf/3`. Tickets, resumption and 0-RTT are disabled.
The connection closes when either verified certificate chain expires. The
ordinary length-prefixed JSON hello follows, with version 3, MTU 1320 and
datagram limit 1100. Subsequent frames contain a one-byte kind and four-byte
big-endian payload length: kind 1 carries a control message including its
existing four-byte JSON length; kind 2 carries a Babel or IP-fragment datagram.
Control messages retain the 64 KiB bound and IP packets the 1320-byte bound.

The listener admits at most 32 pending or active connections, eight concurrent
preface/TLS handshakes, and one active connection per authenticated leaf.
Unauthenticated prefaces cannot reserve or replace a device's active session.
The preface has a five-second deadline and the combined handshake an eight-second
deadline. Cancellation closes connections and joins the routing/TUN workers.

The Linux receive path limits a leaf to its own signed manifest, certificate
bundle and exact derived /128 route. The shared verifier still checks signatures,
revision, router ID, sequence number and lease expiry. Third-party announcements,
aggregate routes, foreign next hops and wildcard retractions are rejected.
Wildcard route requests and same-tenant identity lookups remain available for
endpoint discovery. These checks use the same Babel decoder as the routing
engine, including compressed prefixes and packet-local context.

After transport reassembly, incoming IPv6 packets must carry the leaf's exact
source ULA and a mesh destination. Outgoing packets must target that leaf's ULA.
This first endpoint profile supports TCP, UDP and ICMPv6 without IPv6 extension
headers. Its MTU is 1320, so endpoints must obey path MTU limits instead of
emitting IPv6 fragments. The carrier fragments a valid full-size packet into
at most two frames. Raw IPv4 and IPv6-in-IPv6 remain refused. IPv4-in-IPv6 is
admitted only for the Internet-sharing lease pool, with an IPv4 header without
options or fragments, and TCP, UDP or ICMP payload. The outer source on ingress
and destination on egress must still be the authenticated leaf's ULA. This does
not admit leaf transit. The gateway separately requires its same-tenant mTLS
lease and enforces the exact assigned inner address in its tunnel firewall.

Lite's optional Cloud borrower uses this path for automatic Internet fallback.
It authenticates the gateway with `wendy-mesh-internet/1`; Cloud TLS remains end
to end. See the companion firmware's signed-mesh-leaf documentation for the
borrower configuration and hardware validation limits. The gateway allows a
bounded 60-second mTLS handshake so certificate chains can cross a BLE link.
Lite operator setup over the Cloud relay also permits 60 seconds in the CLI
and companion firmware. Caller cancellation, certificate expiry and signed
session-grant expiry still apply; the gateway does not terminate operator TLS.

The BLE IP queue retains its 16 KiB cap and per-source fairness. Its minimum
allows two full packets plus a short ACK for each of four senders, so an ACK
does not evict its own waiting data packet. Packet age
allows the bounded queue plus one active packet to drain at the observed rate,
capped at five seconds; before a rate sample it allows two seconds. A faster
socket write raises the drain estimate by at most 25% so it cannot immediately
expire another sender's waiting packet. This prevents a queued TCP segment from
expiring while the previous segment is still being serialized on a slow radio.
At overflow, TCP data keeps its earliest queued segment within the same flow,
including lease-pool IPv4-in-IPv6 traffic. Earlier retransmissions may replace
later data; other flows and senders retain the existing queue limits and
scheduling policy.
The stream writer gives directory/Babel the first turn, then alternates with
waiting IP packets. A proof backlog cannot monopolize the writer, and all
fragments of an admitted IP packet are still written together.

The leaf protocol exchanges complete currently verified certificate bundles,
including replies to identity lookups. Certificate descriptors and chunk requests
remain specific to full-router links; Lite does not implement that exchange.

Leaf links do not implement catalogue relay. They are excluded from catalogue
peer registration; service inventories, repair and freshness proofs use full-router
neighbours only.

This is a connection role. Current device certificates do not attest Lite
hardware or prevent the same identity from implementing another enabled
full-node carrier. The listener is configured explicitly, in the file or through
`ConfigureLocalMesh`; nothing enables it automatically. Routers do not advertise
the leaf port: Lite finds routers through the LAN carrier's `_wendy-net._udp`
records and dials port 43024.

`TestLeafRoutedEndpointIsolatedNamespace` verifies the configured TLS listener,
signed route admission, full-MTU UDP and kernel ICMPv6 replies through the actual
TUN/FIB, duplicate/source rejection and teardown. It runs inside an isolated
Linux network namespace with NET_ADMIN and TUN. A container with read-only
sysctls can supply the required IPv6 interface defaults and forwarding at
creation; the router checks already-correct values without rewriting them.
`TestLeafCManifestInteroperability` uses `WENDY_LITE_FIXTURE_TOOL` to verify the
production C encoder/signer against the Go directory verifier, including integer
precision, tampering, expiry and replay across restart.

## Identity and addressing

The complete principal
`spiffe://wendy.sh/tenant/<tenant-uuid>/device/<device-uuid>` identifies a device.
Credentials, certificate caches, directory records and configured peers use
that identity. Cloud's `asset_id` carries the same certificate device UUID.
Authenticate its full principal and tenant before using it as a network
identity; a database field alone grants no authority.

Each wendy net verifier may reuse successful certificate-path signature verification
for up to one minute of monotonic time, within 256 entries and 8 MiB of certificate
bytes. The key includes every presented DER entry, including unused issuers.
Current leaf validity, the verified path's expiry, full principal and tenant, and
the configured identity authorization callback still run on every lookup. The
existing `checkRevocation` policy limits leaf validity windows; it does not query
a dynamic Cloud revocation service. Rollback of the verifier's actual clock
forces a fresh path check. Concurrent callers' snapshot timestamps retain their
own validity bounds without evicting newer proofs or extending cache lifetimes.
Failed checks are not cached, and new credentials or trust create a fresh cache.

The ULA address contains `fd45:b891:567f::/48` followed by the last ten bytes of
the device UUID. Each peer TUN carries the complementary `fe80::1` / `fe80::2`
pair derived from full UUID ordering. The Babel router ID comes from a permanent
interface MAC when available, otherwise a tagged random value. Safety state
persists it with the device principal and Babel feasibility history across
restart; it is not a certificate identity.

## Route admission

Every finite remote route requires a current version-3 manifest signed by the
destination device. The manifest binds its full certificate principal to its exact
Wendy Net /128, Babel origin router ID, and origin sequence number. An authenticated
neighbour can relay that proof unchanged, but cannot sign for another device,
advertise an aggregate, or advance another origin's sequence number. Version-1
and version-2 manifests are rejected, including directory-only presence.

The signature preimage is the bytes `wendy-local-mesh/manifest/3` followed by a
NUL byte, the canonical full principal's UTF-8 byte length as a four-byte
big-endian unsigned integer, those principal bytes, and the exact JSON body
bytes. Ed25519 signs this preimage directly; P-256 and RSA sign its SHA-256 hash.
Binding the complete principal prevents a proof being relabeled into another
tenant even when both tenants trust the same key and device UUID. The JSON body
is not reserialized for verification.

Nodes publish a 60-second lease every 20 seconds, capped at the identity's
certificate expiry. Receivers reject leases longer than two minutes and issue
times more than five seconds in the future. The origin's persistent revision
prevents rollback to an older announcement. A Babel sequence change is persisted
and signed before its updates are sent. Proofs travel over the existing reliable
control stream; Babel datagrams remain unchanged. If a datagram arrives first,
it is dropped and periodic Babel updates repair the ordering.

Admission checks both new updates and retained route candidates. Expiry,
withdrawal, or loss of certificate trust removes eligibility; the one-second
maintenance tick bounds expiry reconciliation while the event loop is running.
An accepted lease has an in-process monotonic deadline, and observed expiry
cannot be undone by replay. Timestamp validation when first receiving a proof
and after process restart still requires reliable wall time. Conflicting address
claims by different principals fail closed.
Router IDs alone confer no authority over another device's address.

These proofs authenticate the origin's address and recent authorization. They
do not prove an honest path metric, current reachability, forwarding, or payload
source authenticity. A relay can still drop traffic or replay a proof within its
valid lease. Hop-local Babel retractions remain unchanged: they only remove that
neighbour's candidate. Babel feasibility, split horizon, loop prevention, and
end-to-end application authentication still apply.

Router QUIC links require hello version 2, ALPN `wendy-net-router/2` and the
authenticated IP carrier capability described below. BLE TLS uses hello version 1
and `wendy-local-mesh-ble/1` without router IP capabilities. Both router carriers
include signed route authorization, neighbour service catalogue, certificate
exchange and HAVE/WANT dissemination. Signed manifest and service schema versions
remain independent. The opt-in leaf listener requires
`wendy-local-mesh-leaf/3` and the `WML3` preface. Earlier BLE and leaf protocols
are rejected during negotiation. Every carrier rejects earlier manifest versions
for both directory presence and route admission, including peers that still
negotiate the preserved router protocol. No automatic downgrade is attempted;
upgrade each connected Wendy Net network together.
This changes the internal Wendy Net wire
contract, not public gRPC messages or application-session ALPNs.

Manifest projection, route authorization and synchronization require the exact
currently admitted wire signature to verify under the principal's current cached
certificate. A same-principal key renewal cannot lend trust to a signature made
with the old key. Same-key renewal can retain a valid signature, subject to the
current chain lifetime and the original signed lease. Re-signing an identical
body with a new key can replace an invalid old signature without renewing its
lease or replay receipt. Queued exports use that exact wire and its matching
current certificate chain together. Unchanged leaves reuse only the signature verdict;
current chain trust and expiry are still checked on every read.
A warm receiver may still hold a previous key while fetching an explicitly
advertised replacement chain. Only that active same-neighbour exchange can
defer a signature failure for a different leaf, or a manifest lifetime beyond
a different exact cached chain (including renewed issuers behind the same
leaf). The bounded pending wire keeps the exact descriptor and its first
deadline, retaining the original monotonic fetch allowance independently of the
signed absolute expiry. Replacement or timeout discards it,
and successful chain completion still requires normal signature, lease and
replay verification. Invalid signatures and overlong certificate lifetimes
outside that transition, or after replacement verification, remain fatal.

## Control and payload flow

Router QUIC uses ALPN `wendy-net-router/2` and hello version 2. Its authenticated
hello keeps the tunnel MTU at 1320 and baseline datagram limit at 1100, and
independently advertises an IP carrier receive limit of 1100 or 1329. A complete
tunnel packet can use one nine-byte-header carrier datagram when the peer and
current validated QUIC path support it. Otherwise the original 1091-byte fixed
fragment positions apply. QUIC starts at a 1200-byte UDP payload and may probe
up to a 1400-byte ceiling; application QUIC and tunnel/container MTUs are
unchanged. BLE and leaf stream protocols retain their separate version and
framing, and do not advertise the router capability.

Nonblocking datagram admission drops on queue pressure without withdrawing a
healthy routing link. A synchronous whole-frame size refusal may fall back
before any piece was admitted; a partially admitted fixed-form packet never
restarts. Missing pieces remain ordinary IP loss. Accepted enqueue is not a
wire or delivery receipt.

For bounded operator diagnostics, setting `WENDY_NET_CARRIER_DIAGNOSTICS=1` in
the agent environment before startup emits a `wendy net carrier stats=` JSON
journal row at most once per active router-QUIC link every five seconds. The
version-1 row identifies the authenticated self/peer and link ID and includes
`tx` configured packet/IP body, negotiated peer IP body and current QUIC body
capacity plus enqueue/drop counters, and
`rx` reassembly counters and bounds. `CurrentDatagramKnown` distinguishes an
unavailable capacity query from a measured value. `Fragments`/`Bytes` in `tx`
count accepted IP datagram bodies including their carrier header, not wire
bytes; `WholePackets` includes already-small packets. `FragmentedPackets`
counts attempts whose first fixed fragment was accepted, including partial
loss. `QueueDrops`/`SizeDrops` include IP and Babel refusals; `WholeFallbacks`
counts size-refused whole attempts that try the fixed form. Counters reset on
link replacement. Diagnostics are off by default and add no wire traffic.

BLE first contact always performs a full TLS handshake because discovery has
only a UUID hint. Once the certificate, tenant, self-identity, hint and ALPN
checks succeed, its post-handshake ticket is collected into the verified full
principal's existing bounded session cache. The first reconnect to that known
peer can therefore resume. An unidentified radio never receives a cached ticket.
Resumed links still recheck identity authorization; ticket expiry, eviction or
credential replacement falls back to a full handshake. The previous connection
must have processed its session ticket before disconnecting.

Each authenticated QUIC neighbour link has one bounded reliable control stream
and independent Babel/IP datagrams. TLS byte-stream adapters multiplex those
frame types through one bounded priority writer, with separate bounded IP
queues and packet-age limits for slow carriers. QUIC permits a certificate
flight of at most 64 KiB per encryption level to carry the enrolled Cloud v2
chain; chain verification and principal pinning remain mandatory. WendyOS explicitly configures that bound through the pinned
[Wendy QUIC fork](https://github.com/wendylabsinc/quic-go/blob/wendy-v0.62.0/WENDY.md);
the upstream default remains16 KiB.

Signed device manifests advertise name, agent port, revision and an absolute
expiry. Version 3 also binds the signed route origin; application services use the
separate catalogue protocol.
Every receiver verifies the origin's certificate and signature before relaying
an accepted version to its other neighbours. Missing certificates cause a
bounded pending record and an identity request to the sending neighbour.
Identity announcements carry an ordered list of SHA-256 certificate hashes.
The receiver reuses individual certificates from its own enrolled chain,
authenticated handshakes and previously verified identities. It requests only
the first missing chain element, in sequential chunks of at most 1,024 bytes.
Concurrent directory/service caches and neighbour links can share the same
bounded byte pool; only the first announcing supplier is asked. Disconnect
releases its request immediately, and a silent supplier yields to another
announcing neighbour after 30 seconds without progress. Routing and application
frames can be transmitted between certificate chunks.
Requested directory replies use a separate reserve of32 frames/64 KiB while
the control writer is full. They drain before new gossip; admitted frames keep
their original order. Protocol overload still closes the bounded peer link.

The assembled chain is independently verified under current trust, principal,
tenant, expiry and renewal rules before admitting its manifest. Hashes identify
bytes only; fetched intermediates never become trust anchors. Known bytes do
not bypass verification after trust changes.

The additional RAM pool retains at most 8 MiB, including reserved unfinished
downloads, 4,096 completed elements and 128 pending element fetches. An element
is limited to 64 KiB. Unverified completed elements expire after two minutes;
verified elements last at most 24 hours, capped by chain expiry. Existing disk
identity caches still persist verified full chains with their own 1,024-entry,
8 MiB and 24-hour bounds. Restart can reuse those chains; unverified fragments
are never persisted.

Certificate exchange is part of each role's advertised control protocol.
TLS handshake certificates and their verification remain unchanged. The endpoint-only
leaf carrier keeps its separate ALPN and hello version 3; the router bridges its
verified announcements into router HAVE/WANT dissemination.

Changed records immediately advertise a HAVE digest. Duplicate revisions do not
renew their expiry. Reconciliation advertises live digests periodically to repair
reconnects; full signed records are sent only on request between routers. A repair
pass uses only free control-queue slots and resumes from its last admitted
principal on the existing maintenance tick; it stops when the pass completes.
Insufficient queue space does not consume a certificate-description stamp.
The node currently signs a fresh presence manifest every 20 seconds, with a
one-minute lease; receivers allow at most two minutes. Reconciliation runs every
30 seconds. These intervals are separate from Babel's one-second
Hello and four-second Update intervals. Durable revisions and bounded replay
receipts prevent restart from resurrecting superseded announcements.

The payload path is IPv6 through peer TUNs and the selected Babel next hop. A
1320-byte logical packet uses the negotiated whole form when current capacity
permits, or bounded fixed-position fragments reassembled at the neighbour.
Raw IPv4 is rejected. A scoped policy table and
unreachable guard prevent missing routes from falling through to the ordinary
uplink. Interfaces disable automatic address generation and router
advertisements. Startup checks all reserved links, rules and routes for
ownership before reclaiming a crashed instance's state; foreign state is
refused intact. IPv6 forwarding has a shared crash journal and scoped firewall
grant so enabling transit preserves the host's router-advertisement behaviour.

Receive reassembly reserves at most 32 incomplete packets and 42,240 bytes per
link. Missing halves expire ten seconds after the first accepted piece;
duplicates do not extend that allowance. A full receiver evicts the oldest
incomplete packet so fresh traffic can progress. A fixed 1,024-ID serial window
suppresses retired IDs and stale unknown pieces across sequence wrap, while a
retained pending packet may still complete after newer traffic advances the
window. The receive owner expires idle buffers without closing a healthy
carrier. Per-link `Reassembly` snapshots report received IP fragments/bytes,
completion, duplicate/stale/invalid pieces, expiry/eviction/discard counts and
current/peak reservations. These are hop-local receive counters, not app-message
metrics or proof of delivery.

Application catalogue gossip uses the separate protocol described below; it
should not be inferred from the device presence directory. Internet-sharing
overlays and LAN service projection have their own policy and runtime layers.

## Cloud relay presence

Cloud relay presence checks its TCP source address every ten seconds. If that
address disappears when a borrowed interface is withdrawn, presence ends,
invalidates its old lease and reconnects through the current route. When the
source remains assigned, a changed preferred source must persist for two checks
and pass a broker TLS transport probe before reconnecting. Failed probes back
off to five minutes; they do not delay offers or lease expiry.

Gateway DNS listens on the device ULA and accepts mesh ingress. `HostPolicy`
now only manages resolver selection and restores its owned resolver changes.
The kernel feature contract is checked by WendyOS-Builder's
`recipes-kernel/linux/mesh-internet.inc`.

## Shared HAVE/WANT announcements

`netgossip.Pool` is shared by every router neighbour and both announcement
classes through `ControlHub.Announcements()`. Transport-specific synchronizers
only adapt signed device manifests and signed service records. A service record
includes publications, renewals and withdrawal tombstones; a device manifest
includes presence, signed router authorization and withdrawal revisions.

The artifact ID is SHA-256 over the domain `wendy-mesh-artifact/1`, its one-byte
type, a big-endian four-byte envelope length and the deterministic signed JSON
envelope. The domain ends with NUL. The signature is included and forwarded
unchanged. Digest fields carry 32 raw bytes, base64-encoded by JSON, not hex.
Each gossip message has version 1 and an explicit artifact type:

- `have`: availability hint containing a digest, with no signed body.
- `want`: digest plus a nonzero request ID scoped to the neighbour session.
- `data`: matching digest/request, total bytes, offset and at most 1,024 bytes.
- `unavailable`: matching request whose body is no longer available.

If B and C advertise the same object to D, D selects only one supplier. The
other remains an alternate. Complete bytes awaiting origin verification retain
the claim, so certificate repair cannot start another full download. Only a
verified, admitted object is advertised onward. Unsolicited full records are
rejected on router links. Digests, availability hints and download completion
never advance signed revision/generation receipts or grant route authority.

Disconnect immediately releases an owner's claim. Thirty idle seconds without
an accepted DATA chunk permits an alternate; polling and repeated HAVEs do not
extend the timeout. Request IDs ignore responses from a superseded download.
Normal delivery downloads each object once; timeout recovery can still receive
late bytes already queued by the previous supplier. Small bounded chunks expose
progress without waiting for a whole large announcement or adding a round trip
for every chunk. A poll emits at most eight chunks and four new requests.

The shared pool bounds known receipts to 4,096, pending objects to 128, suppliers
per object to 64, active downloads/uploads to 16 each, and active transfers per
session to four. Envelopes are at most 40 KiB and cached payloads at most 8 MiB.
Evicting payload bytes retains the verified receipt until signed expiry, so a
new HAVE does not redownload an already admitted object. A serving router can
recover evicted bytes with a keyed authoritative-store lookup retained in its
bounded verified receipt. Unknown requests never scan the directory/catalogue.
Authenticated stale bodies receive short suppression receipts; expired or
unverified certificate-wait bodies release their claim without a shared receipt. Advertised
hints expire after two minutes. No cache is seeded from durable generation
receipts when the actual body is absent after restart.

Offers, delayed WANTs and authoritative manifest loaders require the exact
currently admitted signed record and its current trusted chain. Requested DATA
is preceded by that exact chain's descriptor, at most once per chain during the
certificate fetch lease; a changed chain refreshes the hint immediately. HAVE
still occupies one queue slot. Already queued summaries may receive UNAVAILABLE
after renewal, and already queued DATA must pass ordinary current identity,
signature and signed-lease validation at the receiver before it grants authority.

A known manifest digest with cached verified bytes may repair a missing trusted
identity without another body download. This uses the existing bounded pending
identity exchange and preserves its original lease and replay floor. A currently
trusted replacement key never lends authority to an obsolete signature. If the
body was evicted and its authoritative loader cannot validate current trust,
repair waits for a later publication or lease cycle; no directory scan or extra
artifact metadata is retained for this case.

First delivery adds a request round trip per relay hop; larger bodies also drain
through the bounded chunk scheduler. Presence/route-proof replies use the
existing high-priority directory queue; service replies use their bounded reply
reserve. Both retain application packet fairness. The existing immediate route
reevaluation runs after proof verification; early Babel updates can be rejected
until their requested proof arrives and periodic Babel updates repair ordering.

## Verification

From the WendyOS root:

```sh
go test -race ./go/internal/agent/localmesh ./go/internal/agent/hostnetwork
go vet ./go/internal/agent/localmesh ./go/internal/agent/hostnetwork
```

Linux tests with `WENDY_LOCALMESH_ISOLATED_TEST=1` change interfaces, routes and
forwarding. Run them only inside a disposable privileged container/network
namespace, never in a host or Jetson management namespace. The container needs
`iproute2`, `iptables`, `iputils-ping`, `/dev/net/tun` and IPv6 enabled. Namespace
tests do not establish physical radio performance.

## Neighbour service catalogue

Service catalogue messages use the existing reliable neighbour control stream,
not a TCP listener or a connection to each routed origin. The catalogue owns a
synchronizer for each authenticated link incarnation. The node exposes a bounded
ControlHub and never runs catalogue signature verification or disk writes in its
routing event loop. Catalogue messages are split into 1 KiB fragments, with a
64 KiB logical-message bound; the neighbour writer interleaves these fragments
with urgent device control. Babel datagrams retain the carrier's existing path.

Every neighbour exchanges its current certificate inventory and a HAVE-only
service snapshot. Accepted origin signatures are preserved on relay. The
existing 15-second repair scan re-advertises current digests; it does not push
full records. Advertised records remain pending until their origin checks
complete, including signatures waiting for certificate repair. Projection
requires the independent origin-signed freshness proof, which installs durable
generation floors before readiness; a slow or newly joined neighbour does not
hide services already proven fresh. Failed queue admission
does not consume a send stamp. Certificate cache misses can be repaired through
other neighbours with bounded, coalesced requests. Directory, authenticated
handshake and catalogue paths share the same public certificate-element pool.
Service announcements and freshness proofs describe certificate hashes instead
of embedding whole chains; only missing bytes are requested, one chunk at a time.
A burst of records from the same unknown origin requests its identity once.
Pending unverified records are bounded to128 records/128 KiB and their signed
expiry, and requested replies have a separate16-frame/128-KiB reserve.
Ordinary signed-record formats,
service leases and device-directory renewal/repair intervals are unchanged.

Projection readiness uses a fresh origin-signed inventory challenge instead of an
origin TLS session. A route loss or process restart invalidates readiness. On
route recovery, a random 256-bit nonce request follows neighbour control links
towards the origin; the signed response follows bounded reverse-path state. The
response binds mesh, full origin/requester principals, nonce, issue/expiry times,
and a complete immutable inventory of the origin's durable per-service generation
and body-hash receipts, including inactive keys. Responses are chunked, bounded to
4 MiB each and 8 MiB retained assembly, and expire after 30 seconds. Generation
fences persist before readiness becomes visible. Newer concurrent publications
are preserved; old active bodies cannot bypass an inactive inventory fence, while
signed withdrawal bodies remain relayable. Request, transfer and queue bounds
limit work on malformed or non-responsive peers. No request creates a routed
socket. These freshness requests occur on readiness recovery, not on each normal
service renewal. An origin without a live route remains unprojected.

Each neighbour ALPN selects its complete role-specific contract described
above, so connected routers must be updated together. There is no fallback to
full-record push.
Origin-signed route proofs retain their existing signature contract. Certificate
inventories and origin freshness challenges remain separate from HAVE/WANT.

Run the native affected-package race checks and the opt-in
`TestNeighbourCatalogueLinuxNamespaces` in a disposable privileged Linux container.
The latter creates four real routers in separate network namespaces with only
A-B, B-C and C-D links, checks multihop catalogue readiness and withdrawal after
partition/heal, and asserts no catalogue TCP listener on port 43022.


## Application service discovery

Applications with the mesh network entitlement can browse signed Wendy Net
services over their private DNS-SD bridge. Remote services appear while the
origin has a live host route, a current device announcement and a reconciled
service inventory. Route loss withdraws their DNS records without renewing the
signed service lease; recovery projects only services still present and live.

When Ethernet or infrastructure Wi-Fi is enabled for Wendy Net, the same app
bridge also exposes services observed directly on those physical links. These
LAN advertisements are unsigned and remain local to this device. Each complete
DNS-SD service must name its on-link sender as its IPv4 endpoint. Projection
installs an app-specific /32 route and forwarding grant for the advertised TCP
or UDP port. Other ports and unsolicited LAN-to-app traffic remain blocked.
App-owned names and signed Wendy Net names take precedence over LAN names.
Goodbyes, lease expiry, interface loss and application shutdown revoke grants;
agent startup revokes inherited grants before reauthorizing current services.

LAN browsing excludes router discovery services (`_wendy-net._udp` and the
former `_wendy-mesh._tcp`) from both app records and follow-up type queries.
Ordinary app UDP services remain eligible, including those sharing a router host.
LAN browsing currently supports IPv4 endpoints and excludes Wendy Net carrier,
underlay and service address ranges. Namespace tests cover scoped forwarding,
withdrawal and coexistence with host Avahi; physical-link acceptance remains a
separate validation step.
