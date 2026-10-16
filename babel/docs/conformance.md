# Validation ledger

This is an implementation/test ledger, not a protocol certification. References:
[RFC 8966](https://www.rfc-editor.org/rfc/rfc8966.html),
[RFC 9229](https://www.rfc-editor.org/rfc/rfc9229.html), and
[erratum 7373](https://www.rfc-editor.org/errata/eid7373).
Specification snapshots and hashes live in the project `nan/specs` working area.
7373 was Held for Document Update at the 2026-09-11 audit, not Verified.

## Requirement groups

The table groups related normative clauses; it does not yet individually enumerate
every SHOULD and MAY. Full clause-level review remains a stable-release gate.

| Requirement / reference | Implementation | Evidence / qualification |
| --- | --- | --- |
| §3.1, §4 native transport/source/port/hop limit | UDP oracle adapter | Mixed daemon tests; QUIC adapter deferred |
| §3.1 bounded urgent delay | trigger scheduling | 0–50ms initial trigger; delayed repetitions; caller must run deadlines |
| §3.1 pacing/aggregation SHOULD | packet packing, periodic jitter | No general per-packet pacing yet; individual urgent replies immediate |
| §3.2.1 modulo-65536 arithmetic | newer/atLeast | TestSerialArithmetic, TestSequenceWrapRecovery |
| §3.2.2 node seqno changes | seqRequest | TestSeqRequestIncrementOnlyOne; no spontaneous increments |
| §3.2.3–4 scope and histories | LinkID, linkState | TestScopedLinksAndStale, TestHelloExpiryAndUnscheduled |
| §3.2.5 source key includes origin | sourceKey | Different-origin/default simulation |
| §3.2.6 route key includes neighbour | routeKey | Diamond/alternate-path simulations |
| §3.3 required ACK reply, unicast | receive/queue | TestUnknownPrefixRequestRetractsAndAcks |
| §3.4 neighbour before routes | explicit AddLink | No unauthenticated auto-discovery in core |
| §3.4.1 independent Hello kinds/promises | helloState[2] | Vectors and expiry tests; multicast-kind outbound oracle fix |
| §3.4.2 IHU destination and expiry | receive, cost | Silent loss/recovery oracle and partition simulation |
| §3.4.3 positive cost, Hello/IHU infinity | cost | Link-loss tests and fuzzed asymmetric costs |
| §3.5.1 feasibility against advertised metric | feasible | TestHoldDownAndFeasibility; small-graph forwarding assertions |
| §3.5.2 positive saturating additive metric | sum, metric | TestSerialArithmetic and independent shortest paths |
| §3.5.3 unknown retraction ignored | update | Withdrawal scenarios; selected infeasible route unselected, not ignored |
| §3.5.3 route expiry then flush | expire | Long-running simulations; stale-route recovery |
| §3.5.4 no covering-prefix fallback | drops, Effects.Routes | TestOverlappingDefaultHoldDown; commit contract |
| §3.6 finite feasible selection; no seqno preference | selectRoutes | Small-graph tests, fuzz; equal-cost stickiness only |
| §3.7.1 periodic selected advertisements | timers/dump | Loss/rejoin tests and real babeld |
| §3.7.2 urgent origin change, bounded repeats | update/selectRoutes/timers | Multiple-origin simulations; broader timed-clause audit still needed |
| §3.7.3 FD update before finite sends; no reset on retraction | advertise | Feasibility and restart checkpoint tests |
| §3.7.4 split horizon conditions | advertise | Explicit symmetric point-to-point profile; request replies bypass |
| §3.8.1.1 exact requests answered even if absent | receive/advertise | TestUnknownPrefixRequestRetractsAndAcks |
| §3.8.1.1 wildcard rate limit SHOULD | per-link lastDump | Bounded dump interval; broader abuse tests pending |
| §3.8.1.2 seqno reply/increment/one-neighbour forwarding | seqRequest/request | Sequence tests, multihop recovery; focused duplicate/hop tests |
| §3.8.2.1 starvation MUST request, retries SHOULD | selectRoutes/request | Lost feasible route test and graph recovery |
| §3.8.2.2 + erratum no-selected-route recovery | update | TestErrata7373NoSelectedRoute |
| §3.8.2.3 pre-expiry request SHOULD | timers | Selected-candidate refresh scheduling |
| §4 body framing and trailer isolation | wire.Decode | Independent vectors, truncation, fuzz |
| §4.1 big endian, centiseconds, ID validity | wire codec, New | Vectors, sequence boundaries |
| §4.3 unknown TLV ignore | wire.Decode | Fuzz and malformed vectors |
| §4.4 mandatory/optional sub-TLV semantics | subTLVs | Context/missing-prefix tests |
| §4.5 context even when enclosing TLV ignored | wire.Decode | TestParserContextAndMandatory, TestNextHopMandatoryState |
| §4.6 reserved flags ignored on receipt | wire.Decode | Base vectors; no security meaning assigned to reserved bits |
| §4.6.9 prefix masking/compression/R flag | prefix, Decode | Context vectors/fuzz; encoder does not compress |
| §4.6.9 missing ID/next-hop ignore; wildcard withdrawal | Decode/update | Wildcard vector and simulator |
| §4.6.10–11 wildcard/seq-request validity | Decode | Parser fuzz, malformed input tests |
| RFC9229 §§2,4 AE4 separate compression, IPv6 hop | codec, advertise/update | AE4 vectors + actual Linux IPv4-via-IPv6 forwarding |
| RFC9229 §2.2 no selection without capability | Config.IPv4ViaIPv6 | Explicit gate, adapter asserts capability |
| RFC9229 §2.3 AE1 request preference / AE4 acceptance | codec | Encoder emits AE1 requests |
| RFC9229 §3 ICMPv4 even on IPv6-only links | application responsibility | Oracle uses Linux forwarding and loopback IPv4 addresses |

## Test interpretation

The simulator applies a full FIB snapshot atomically, walks packets using longest
prefixes, and tests for single-origin loops at event boundaries. It deliberately
does not promise loop-freedom for multiple origins of the same prefix. It checks
recovery after bounded delivery resumes. Its independent shortest-path calculation
checks stable graphs, not instantaneous shortest paths during convergence.

Four-node graph enumeration explores all edge subsets and selected event sequences,
not all possible schedules. Fuzzing adds event timing, drops, duplicates, reordering,
origination/withdrawal and cost changes. Fuzz campaigns and successful interop are
evidence, not a formal proof. Forwarder transition behavior and protocol state are
separate concerns; kernel adapters cannot assume globally atomic route installation.

## Release gates still open

- Independent review of the complete normative-clause ledger and safety model.
- Longer fuzz/churn/overload campaigns, minimized failing traces if found, and
  mutation testing to demonstrate that the harness detects intentionally broken rules.
- Second independent implementation oracle (babeld is currently the only reference).
- More mixed-daemon topologies, asymmetric metrics and origin/next-hop changes.
- Measured 1,000-node *full host-route* workload (the initial 1,000-node test has
  three origins, not 1,000). Do not generalize those results to every mesh workload.
- Actual Android shared-library/JNI and iOS app/device lifecycle tests, stable ABI,
  suspend/resume policy and crash-safe checkpoint integration.
- Authorization/filtering adapter before deployment on an untrusted mesh.

These are reasons to label the library experimental, not reasons to couple it to
the existing NAN backend prematurely.
