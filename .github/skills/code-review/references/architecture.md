# Architectural checks

These checks come from WendyOS review history from August through November 2026.
Verify current ownership and accepted decisions before applying a historical lesson.

## Put the fix in the component that owns the invariant

Trace the failure back to its owner, rather than reviewing only the proposed remedy.
Investigate the CLI for interaction and orchestration, the agent for device services
and resource lifetime, WendyOS-Builder for image and OS configuration, and shared
Cloud/PKI code for identity contracts. These are starting points, not a ban on one
subsystem compensating for another.

Watch for CLI refusals or agent retry loops that hide an OS layout, clock, DNS or
network configuration defect. Compare with an owner-level fix and account for Wendy
Lite and non-WendyOS hosts. A temporary workaround may still be justified when its
scope, recovery behavior and removal condition are clear.

Evidence: [#2014](https://github.com/wendylabsinc/WendyOS/pull/2014),
[#2231](https://github.com/wendylabsinc/WendyOS/pull/2231),
[#1770](https://github.com/wendylabsinc/WendyOS/pull/1770).

## Avoid parallel services and sources of truth

Before accepting another service, socket, entitlement, enrollment flow or background
manager, identify the existing equivalent and what cannot be expressed there. Ask
who creates, renews, revokes and cleans up each piece of persistent state.

- Sensor annotations may need recording, rules and independent subscriptions. A
  thermal-only video field commits every video client to a special case and may
  bypass those data consumers.
- Model execution can need a new backend without needing another public lease,
  watch and campaign control plane.
- An uploader should consume the authoritative enrolled identity when that meets
  its contract. A second enrollment client creates conflicting credentials and
  recovery rules.
- A new sensor socket may duplicate native device access and existing capability
  brokering. Check what isolation or semantics the extra socket adds.

Evidence: [#2434](https://github.com/wendylabsinc/WendyOS/pull/2434),
[#2063](https://github.com/wendylabsinc/WendyOS/pull/2063),
[#1794](https://github.com/wendylabsinc/WendyOS/pull/1794),
[#1973](https://github.com/wendylabsinc/WendyOS/pull/1973).

## Keep identity and authority consistent through the complete flow

Trace selection, discovery, enrollment, connection, renewal, unenrollment and reset.
Compare USB, LAN, BLE, Cloud and mesh paths that the change actually affects.

Look for hardware-derived identity after a UUID migration, different Cloud sessions
used for different legs of one operation, pin cleanup racing a concurrent update,
and local reset after a lookup miss while an asset and certificate remain active.

Authentication is not sufficient authorization. Check the registered server,
interceptors and per-method/resource checks before claiming either that a handler
is unprotected or that a valid certificate grants the right authority. A hosted
gateway should not acquire organization-wide privileges merely to proxy a user's
request. Bound tokens require the proof specified by the accepted protocol.

Do not prescribe weaker certificate checks or silent protocol downgrade to make a
workflow pass. Equally, do not invent a mandatory restriction without identifying
the threat, affected supported workflow and migration decision.

Evidence: [#2182](https://github.com/wendylabsinc/WendyOS/pull/2182),
[#2429](https://github.com/wendylabsinc/WendyOS/pull/2429),
[#2328](https://github.com/wendylabsinc/WendyOS/pull/2328).

## Treat API shape and rollout order as behavior

Review the relevant old-client/new-server and new-client/old-server combinations.
Include stored records, firmware and published artifacts where applicable.

- Preserve protobuf field numbers and distinguish absence from valid zeroes.
  Wire-compatible types can change meaning: boolean true and enum value 1 encode
  alike but need not request the same operation.
- Use extensible choices for domains with multiple values. Separate read results
  from write inputs when most fields are meaningless in one direction. Reset may
  need to clear persistence, not just write a default value.
- Keep capabilities attached to the device they describe. One aggregate GPU flag
  cannot represent mixed GPUs or distinguish GPU from NPU support.
- Identify the canonical protocol source and generation path. Check affected Go,
  Swift and firmware consumers, not just one language's generated diff.
- Declare minimum versions, capability negotiation and dependency merge order.
  Check the fallback, refusal or migration deliberately. An accepted breaking
  change can be appropriate; an accidental break is not justified by new-client tests.

Evidence: [#1817](https://github.com/wendylabsinc/WendyOS/pull/1817),
[#1818](https://github.com/wendylabsinc/WendyOS/pull/1818),
[#1908](https://github.com/wendylabsinc/WendyOS/pull/1908),
[#2057](https://github.com/wendylabsinc/WendyOS/pull/2057),
[#2664](https://github.com/wendylabsinc/WendyOS/pull/2664).

## Give shared hardware and long-lived work explicit owners

A new capture helper can claim a camera exclusively even when its API promises
sharing. Trace device opens, producer ownership and fan-out across consumers,
including those using older APIs.

For each stream, worker, reconciler or cache, identify who starts it, cancels it,
waits for it and releases it. Check last-subscriber departure followed by immediate
rejoin, service shutdown, blocked I/O and partial startup failure. A context argument
does not prove that a syscall or child process obeys cancellation.

Check that a dead consumer cannot stall healthy consumers, serial retries do not
occupy an entire reconcile loop, and queues, diagnostics and sessions have bounds.
Use negotiated geometry for frame-size checks. A stdout stream must not silently
lose framing when a depth plane disappears. Validate both color and depth lengths.

Evidence: [#2041](https://github.com/wendylabsinc/WendyOS/pull/2041),
[#1976](https://github.com/wendylabsinc/WendyOS/pull/1976),
[#2052](https://github.com/wendylabsinc/WendyOS/pull/2052).

## Preserve legitimate workflows and report the state actually achieved

Examine the workflow a proposed safety rule prohibits. Failed Wi-Fi association
can be intentional during password rotation. A robot mode bit does not prove physical
resistance. A library on disk does not establish executable GPU support.

Keep requested configuration, applied configuration, observed capability, process
start and application readiness distinct. A sealed episode may not be published when
its active recorder disappears. A generic HTTP 200 does not identify the intended app.

Review failures as part of the public interface. Broad rewriting can turn a missing
app into a missing device, erase recovery commands or move structured next_steps into
prose. Treat device names and logs as untrusted terminal input. Preserve literal case
in flags, paths and identifiers.

Evidence: [#2214](https://github.com/wendylabsinc/WendyOS/pull/2214),
[#2035](https://github.com/wendylabsinc/WendyOS/pull/2035),
[#1909](https://github.com/wendylabsinc/WendyOS/pull/1909),
[#2432](https://github.com/wendylabsinc/WendyOS/pull/2432),
[#2022](https://github.com/wendylabsinc/WendyOS/pull/2022).

## Check changes across stacks and repositories

Identify prerequisites, duplicates and stale assumptions when branches overlap.
A shared-source pin to an unmerged dependency needs an explicit integration decision.
Generated volume can obscure a small handwritten contract change; inspect the source
and generation path. Report coverage gaps when review tooling cannot inspect a change.

An unavailable dependency or unrun hardware check does not automatically prove broken
code. Name the missing acceptance evidence. A closed, rebased or superseded PR does
not by itself show technical rejection.
