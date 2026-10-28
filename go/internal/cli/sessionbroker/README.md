# Retained operator mesh broker

This package retains one real userspace Wendy Net participant for the selected
operator context. Commands share its Babel node, IPv6 stack, verified manifests,
carriers and private IPC endpoint. An operator keeps its primary certificate and
role; its persistent installation identity supplies its own 5680 mesh address.
It is not represented as a synthetic enrolled device or a delegated observer.

The existing direct device TLS broker remains available independently. Its
parent-process lease and five-minute default idle policy are unchanged. A full
operator router stays enabled after the last command closes, so peers can still
use it as a transit hop. Explicit disconnect cancels and joins that participant.
The non-secret recipe and installation state remain in the selected persistent
profile; later explicit preparation can rejoin. Observational reads never start
or enable a context. Credential replacement drains the old owner before rejoin.

Private startup and principal-scoped lifetime locks serialize concurrent callers.
macOS and Linux use private Unix sockets; Windows uses per-user named pipes,
owner/ACL checks, file locking and detached process creation. Windows compilation
is covered; runtime qualification remains separate. Each helper owns one context,
not a process-wide collection of unrelated operator identities.

## Carriers and reconnect

Explicit TCP and BLE first-hop recipes authenticate the exact selected WendyOS
peer with the primary operator certificate, tenant chain and profile pins. The
same shared node can own additional authenticated carriers. The bounded scheduler
holds at most 256 candidates, eight connections, two BLE connections and two
concurrent setup attempts. Automatic discovery supplies LAN TLS and host BLE
candidates to the same scheduler. macOS and Linux support outgoing BLE; explicit TCP recipes do not
require radio discovery. The direct first hop must currently be a WendyOS peer;
Lite targets are reached through its routed connections.

A first-hop failure retains the broker's IPC endpoint and installation identity.
The carrier scheduler reconnects while the node remains alive; if the participant
itself ends, the supervisor closes that generation before reopening it. Automatic
recipes can rediscover a changed LAN address. Existing failed flows end normally;
traffic is never replayed silently. Current credentials and pins are checked again
for new handshakes. The shared node supports actual packet forwarding after the
last command lease ends.

## Local observations and target connections

The broker reads its own verified node state. A target requires a live authorized
Babel route joined to its matching unexpired signed manifest. Signed platform,
management port, transport and capabilities select the protocol; they never replace
end-to-end destination authentication or permit a fallback to an arbitrary LAN IP.
Adjacent agent RPCs reuse the authenticated carrier's management stream. Remote
agent connections authenticate the remote device independently and retain the
operator's actual source address.

Private `TopologyV1/Get` returns binary protobuf, up to 256 KiB. It is strictly
observational and preserves exact integer values. `DirectoryV1/List` and `Watch`
project public device descriptions, with at most 256 rows and 128 KiB per reply.
Up to 16 watchers share one one-second poll and a single replaceable snapshot per
watcher. Unchanged observations emit only a five-second freshness heartbeat; the
last watcher cancels pending polling. `ServicesV1/List` projects inherited services
from the same verified manifests, with at most 512 rows and 256 KiB per reply.
Both text projections report truncation within their complete encoded budget,
including long operator identities and watcher metadata. They carry no binary
certificate or signature material and create no separate service gossip/cache.
See [operator_directory.md](operator_directory.md).

The remote agent cache retains at most 16 clients. Active RPC borrowers cannot be
evicted; idle entries are replaced oldest first. Endpoint refresh rechecks the
current route and manifest. Selected target metadata is local-only and stripped
before forwarding. Physical/direct TLS contexts retain their existing behavior.

## Lite and application streams

Lite management uses a bounded native WendyCom adapter over the retained routed
connection; it does not add redundant TLS inside the already authenticated VIP.
Private `AppV1/Open` carries two raw port bytes and admits only a TCP service
currently declared by the exact verified Lite target. It performs the target's
normal TLS/WAS1 application handshake. Filtering to that target before projecting
its services avoids losing it behind unrelated public inventory truncation.

Management, native application and auxiliary TCP streams share eight slots and
16 KiB chunks. Slow consumers apply backpressure. Setup cancellation stops owning
a successfully returned stream; the returned connection owns its subsequent life.
Deadline-aware half-close drains queued data and supports ordered replies, while
Close cancels blocked sends/reads and releases admission exactly once. Auxiliary
TCP selects a port on the verified target; registry TLS remains end to end.
See [operator_lite.md](operator_lite.md).

Installation metadata and counters persist locally, including migration of an
earlier local CBOR field spelling. This is not mesh wire backward compatibility.
The attachment and topology formats remain binary CBOR/DER and protobuf. The
hidden helper's existing base64 process-launch recipe is non-secret local process
metadata, not a mesh packet.

For commands, device selection and supported operations, see the
[CLI mesh guide](../assets/docs/development/local-mesh-radio.md).
Tests in this package cover ownership, reconnect, forwarding, bounded inventories
and stream lifecycle; hardware-specific behavior still depends on the host and
selected radio. Windows TCP and named-pipe support builds, but this does not
establish physical Windows device interoperability.
