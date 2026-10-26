Wendy Net UDP message transport

Each authenticated UDP connection has one destination and two independent
message directions. TCP retains its pooled WAS1 streams. UDP control requests
use WAU2 and replies use WUA2; old WAU1 peers receive status 2 (unsupported).
There is no fallback that could interpret an old payload as a fragment.

All integers use network byte order. The 16-byte request contains magic (4),
destination port (2), zero flags (2), maximum message bytes (4, exactly 65507),
maximum total chunk bytes (2, at least 256), and zero reserved bytes (2).
The 16-byte reply contains magic (4), status (1: denied=0, admitted=1,
unsupported=2, capacity=3), version (1, exactly 2), selected chunk bytes (2,
minimum of offer and 1000), message bytes (4, exactly 65507), and zero reserved
bytes (4). A client rejects invalid capacities, versions and reserved fields.
The ALPN remains wendy-app-mesh/1 because TCP is unchanged.

A chunk starts with a 20-byte envelope: WU (2), version (1, 2), zero flags (1),
nonzero message ID (8), total payload bytes (2), payload stride (2), chunk index
(2), and chunk count (2). Stride is at least 236 and at most selected chunk
bytes minus 20. Count is max(1, ceil(total/stride)); every chunk has exactly
stride bytes except the canonical final remainder. Zero bytes use one empty
chunk. Default stride 980 carries 65507 bytes in 67 chunks; the minimum selected
chunk permits at most 278. Invalid geometry is rejected before allocation.
QUIC authenticates chunks; no separate checksum or reliable acknowledgement is
introduced. A successfully enqueued transport chunk is never retransmitted.

Each direction allocates monotonically increasing IDs, never wraps and keeps a
4096-ID fixed window of unseen, assembling, delivered and dropped states.
Delivered, expired, conflicting and evicted IDs cannot resurrect. IDs below the
window floor are permanently rejected. Identical duplicates do not extend the
absolute 30-second receive lifetime. Metadata or byte conflicts poison the ID.
The same 30-second absolute bound covers outgoing queueing and all local retries.

The node owns one Manager across proxy, client and server. Whole messages reserve
space before copying or fragmenting. RX/TX limits per flow are 4/8 messages and
256 KiB each direction; per authenticated peer 16/32 and 1 MiB; per node 128/256
and 8 MiB. Empty and small messages cost at least 256 bytes. There are at most
256 flows, 32 per peer. Metadata is separately bounded by these counts, a fixed
window and a fixed fragment-presence array. Completed RX messages retain their byte/count reservation until the caller
explicitly releases them after final delivery or discard. Completed ownership
cannot be evicted for new assemblies; concurrent receivers remain within the
same RX limits. Only whole messages reach a local UDP write. At RX pressure,
up to four old incomplete assemblies are evicted, favoring this flow, then this
peer, then an over-share peer, then the oldest global candidate. Terminal IDs
remain suppressed after eviction.

One worker rotates peers, flows and messages, attempting one chunk per visit
(maximum 64 attempts per 5 ms tick/wake). A later small message can overtake a
large one. TrySendDatagram queue refusal retries only local admission after
5/10/20/40/80/100 ms, capped at 100 ms and the message deadline. Queue refusals
never advance the cursor. No goroutine exists per fragment. Locks protecting
queue selection are released before authorization; each final bounded Try call
runs under its exact source/destination token guard. No blocking SendDatagram,
network wait or timer wait occurs under authorization. Before any accepted chunk,
a smaller transport capacity may select a smaller stride. After partial emission,
a size failure drops the remainder; the receiver's incomplete message expires.
TX path reduction never narrows the independently negotiated RX capacity.

Self traffic uses the same whole-message admission and fairness worker, then one
nonblocking local socket write per visit. The kernel must receive the complete
UDP message atomically, so a local message cannot use the transport's byte-sized
quantum. Source and destination incarnation tokens are checked in one registry
read lock; nesting read locks would deadlock behind a queued revocation writer.
Lease notifications immediately retire pending dials and idle flows. Credentials,
route loss, connection close and endpoint close retain their existing cancellation
paths. Close joins the current bounded admission before retiring queued, incomplete
and completed message charges. Message Release is idempotent after retirement.

Linux local delivery uses IP_MTU_DISCOVER=DONT on the agent's connected sockets;
read buffers accept the complete ceiling and MSG_TRUNC/MSG_CTRUNC discard a read.
The agent requests 128 KiB receive and send buffers, then charges actual kernel
SO_RCVBUF + SO_SNDBUF against 2 MiB per flow, 16 MiB per peer and 128 MiB per node.
Linux normally reports twice each requested size: 512 KiB per connected socket.
A 512 KiB allowance is reserved before creating a connected socket and then
replaced with actual kernel values before attaching its transport. Socket
charges retire with the flow. Agent-local limits do not include QUIC's
bounded transport queues (32 send and 128 receive items per connection), shared
listener sockets, or the one shared transparent receive/raw-send pair. At the
1000-byte cap the 256-flow QUIC send/receive payload ceilings are roughly 8/32 MiB,
plus fixed transport metadata. Local full-message read scratch space is at most
about 16 MiB for 256 active local endpoints plus one shared proxy read buffer.

The transparent reply sender uses a raw IPPROTO_UDP socket without IP_HDRINCL,
claims no UDP source port, supplies a complete UDP checksum and uses IP_PKTINFO
to pin the verified bridge and source VIP. Linux generates IPv4 headers/IDs and
fragments at the local route MTU. IP_TRANSPARENT permits the VIP source; a classic
BPF drop filter makes the socket receive no unrelated UDP copies. A nonblocking
send runs under the same source-incarnation check across the whole kernel call,
including generation of local fragments. IP_HDRINCL must not be used here: Linux
rejects over-MTU raw packets rather than automatically fragmenting them.
Applications retain their own PMTU/DF policy. Full-ceiling fixture apps explicitly
allow fragmentation for both requests and replies to the proxy's bridge address;
ordinary 1472-byte traffic at MTU1500 is tested with default application policy.
App QUIC payload1200, mesh TUN1320 and normal container MTU1500 stay unchanged.

Production diagnostics are normal agent journal records. At startup,
`wendy net: UDP message limits` reports manager_epoch and configured protocol,
message/chunk/lifetime/node limits. At most once per 30 seconds, when the fixed
aggregate snapshot changes, `wendy net: UDP message state` reports the same
manager_epoch and a `stats` object. Use the ordinary agent log/journal access;
there is no new RPC, special diagnostic binary or hidden transport injector.
The timestamp epoch distinguishes manager restarts. Counters reset at that epoch.

Snapshot fields RXBytes/TXBytes, RXMessages/TXMessages, Flows and SocketBytes are
current owned charges; RXHighWater/TXHighWater, RXMessageHighWater/
TXMessageHighWater, FlowHighWater and SocketHighWater are lifetime high waters.
NegotiatedChunkMin/Max cover active remote endpoints' selected total chunk caps;
SendChunkMin is their current effective TX floor after size feedback. Zero means
no active remote endpoint. NegotiatedChunkSeenMin/Max and SendChunkSeenMin
retain the observed lifetime range/floor across closed flows, so short exchanges
remain diagnosable on the next periodic snapshot. Configured chunk_max is a
separate startup field.
TransportAdmissions means successful local QUIC enqueue or local kernel write,
not network emission or delivery. LocalDeliveryAdmissions counts successful
complete-message local kernel writes (a subset of transport admissions for self
traffic, plus final destination/source delivery writes). ReceivedChunks counts decoded chunks including
duplicates; ReassembledMessages means full owned bytes handed to the local caller,
not application delivery. BudgetRejects, CanceledMessages (queued, incomplete and completed messages retired on close),
ExpiredMessages, EvictedMessages,
MalformedChunks, DuplicateChunks, LateChunks, ConflictingMessages,
TransportQueueRefusals, SizeDrops, GuardRejects and TransportFailures classify
local decisions. GuardRejects includes source/destination/connection lifetime
guard rejection and closed admission; it does not prove a remote app's state.
No per-peer, tuple, message ID, credentials or payload is logged. Hardware tests
must observe actual changed journal fields to attribute a decision; app echoes
alone cannot prove that a fragment was lost or a budget was hit.
