# Supported profile and application contract

Status: experimental; no stable API or unconditional conformance claim.

## Boundaries

- IPv6 link-local control addresses, explicit one-neighbour links. Each link has an
  opaque monotonically increasing incarnation ID, not an OS interface index.
- Multicast-kind Hello sent by unicast to the sole neighbour. Incoming multicast-
  and unicast-kind histories are independent. Fixed positive receive cost; total
  cost is the maximum of local receive cost, advertised transmit cost, and one.
  Expired Hello/IHU state produces infinity. No ETX or RTT estimator.
- Minimum-metric feasible route selection, retaining the existing equal-cost next
  hop. Sequence numbers affect feasibility, never preference. Dynamic-metric
  smoothing beyond equal-cost stickiness is deferred; use stable costs initially.
- IPv6 routes and native IPv4 next hops; optional RFC 9229 IPv4-via-IPv6 forwarding
  capability. AE1 and AE4 compression contexts stay separate. Outbound requests use
  AE1 for IPv4. No AE4 in IHU/Next Hop. Config capability is an application promise,
  not automatic detection of kernel support or ICMP capability.
- Periodic updates and split horizon, with explicit-request replies exempted from
  split horizon. Triggered changes are repeated up to three times, with spacing;
  no acknowledgment-request generation, but acknowledgment requests are answered.
- Full wildcard dumps are per-link rate-limited. Request retries are bounded;
  duplicate suppression is per source/sequence. Periodic updates provide repair.
- Prefix-withdrawal suppression uses unreachable/drop entries, not acknowledged
  retraction optimization. Drop holds use our advertised update interval; selected
  replacements immediately remove the drop. Drops MUST participate in forwarding
  longest-prefix lookup before defaults. They are not just diagnostic metadata.
- Incoming third-party IPv6 next hops are rejected in the one-peer profile. Native
  IPv4 next-hop reachability and neighbour resolution are adapter responsibilities.
- Scoped/link-local, multicast, loopback and unspecified host destinations are
  rejected as routed prefixes. Default routes remain allowed.
- Unsupported TLVs are ignored; mandatory unknown sub-TLVs suppress their enclosing
  TLV after parser-context updates. Source-specific routes are not supported.
- Original encoder emits self-contained uncompressed updates. Decoder accepts
  compressed advertisements. Compression is an optimization, not a wire-compatibility
  requirement. Native Babel/UDP and QUIC encapsulation are different transport profiles.

## Time, buffers and effects

`Step` receives elapsed monotonic `time.Duration`, not Unix time. Due expiry is
processed before the supplied event. Call `Tick` at `NextDeadline`; failing to do so
breaks timing promises. Do not repeatedly call at arbitrary intervals instead.
The engine does not replay an unbounded backlog of missed periodic sends after a
long pause. Platform suspend should tear down/re-establish links appropriately.

`Step` treats a packet as one atomic input and batches consecutive updates. Requests
within it see preceding changes. All externally observable map iteration is ordered;
random jitter uses a supplied deterministic seed. Malformed payloads are counted and
ignored, not returned as fatal errors. Invalid API events return an error without
advancing time. Link-local zones must be removed from addresses; the LinkID supplies
the scope. `Receive` never retains a reference to its input buffer.

`Effects.Routes` is a full desired snapshot, not a delta. Local entries describe
prefixes the application asserts it already serves. Snapshots describe selected
state; while a revision is pending they are not proof that a FIB has been applied.
No subsequent Step is allowed before Commit. Successful Commit releases buffered
datagrams. Apply the entire batch before sending any of them. Kernel route commands
are not globally atomic: the oracle adapter uses them for testing, while a userspace
forwarder should swap a table atomically. Production kernel integration would need
its own transition safety policy. An arbitrarily slow commit can violate protocol
deadlines: fail closed instead of blocking routing indefinitely.

After failed forwarding application, the engine stops permanently. The embedding
application must also stop packet forwarding, not retain stale routes or fall back
through a default. A replacement engine must restore safety history or use an
explicit cold-start/quarantine policy.

Limits bound link, candidate/local/drop admission, sources and pending requests.
Safety history is never evicted under resource pressure. New routes/requests can be
rejected while existing state continues to expire/refresh. Temporary drop records
can coexist with candidate records for the same prefixes, so MaxRoutes is an
admission budget, not a strict count of all internal structs. Packet input is bounded
by the link's payload budget; outputs are bounded by configured topology/state and
input packet size. Caller queues also require independent bounds. Do not retry a
failed send by calling Step again with the same event; drop the datagram and allow
normal periodic/explicit recovery.

## Restart and security

RouterID must be unique and must not be all-zero/all-one. It is not an authenticator.
For crash safety, persist Checkpoint before releasing outgoing datagrams, including
the local sequence number, source feasibility history and suppression prefixes.
Restore resets safety lifetimes conservatively, but does not restore learnt routes
or silently re-originate prefixes. Reapply origination policy and establish new
links above LastLink. Never restore a stale checkpoint that predates advertisements.
Durability and storage failure behavior belong to the application. Atomic persistence
may be optimized later; the engine does not implement a filesystem journal.

For a first start with no history, New is appropriate. Restarting with New and the
same addresses while neighbours retain old routes is NOT the documented safe restart
path. Merely choosing a new RouterID does not restore lost history for remote origins.

No cryptographic authentication is implemented here. The optional `AcceptRoute`
callback lets the embedding application authorize origin/prefix pairs on admission
and selection; a policy change takes effect at the next `Step`. Nil preserves the
unfiltered experimental profile. This callback must be deterministic for each
event and must not re-enter the engine. The caller authenticates each link and
supplies its own authorization policy before using this on an untrusted mesh. In particular, mTLS to
a neighbour does not prove the origin of its relayed announcements. Do not claim
protection against a malicious enrolled router or duplicate router IDs. Self-origin
updates are ignored; identity collisions are not automatically resolved.

The UDP oracle verifies source port and scoped peer address, joins Babel multicast,
and sets outbound hop limit to one. Production UDP adapters also own destination,
interface, packet-size and address validation. QUIC adapters own framing, session
identity, replay behavior and payload limits. The library neither validates mTLS nor
knows about an organization's asset/VIP assignments.

## Explicitly outside this implementation

IP forwarding and ICMP, TUN/TAP integration, MTU adaptation, NAN/QUIC session formation,
uplink probes, NAT, DNS, signed gateway authorization, VIP assignment, discovery and
mobile VPN/background policy. No OS image or Builder changes are needed to use or
test the isolated library. Hardware internet-sharing integration remains deferred.
