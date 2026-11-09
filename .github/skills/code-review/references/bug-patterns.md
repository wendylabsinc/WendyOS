# Recurring bugs and the fixes behind them

## Scope and evidence

This analysis uses the August 4 through November 4, 2026 PR audit snapshot. The
inventory contains 857 PRs merged during that window. Title screening identified
286 potential bugfix PRs, followed by examination of the descriptions of the 36
representative PRs linked below. These are recurring mechanisms, not an exhaustive
bug inventory or a statistical ranking. Feature PRs can contain fixes, and a merged
PR can target an integration branch.

Descriptions supply the reported incidents, causes, fixes and validation limits.
The audit also inspected historical implementation changes for #1716, #1945,
#2012 and #2044, and fragmentation regression tests in #2452. Reported device tests
were not rerun for this analysis. Check current code and subsequent fixes before
using a historical failure as a review finding.

Select sections by the changed behavior:

| Change | Sections |
| --- | --- |
| Deploy, stop, restart or resource ownership | Replacement, restart recovery, background work |
| Video, audio, ROS 2 or episode data | Background work, framing, timestamps, durable state |
| Discovery, connections or Cloud | Discovery, identity, failure classification |
| Hardware, OTA or flashing | Capabilities, flash state |
| Configuration, build or public APIs | Representation drift, capabilities |

## Replacement needs an explicit transition

Container replacement destroyed the running app before image, configuration and
entitlement checks could fail. [#2354](https://github.com/wendylabsinc/WendyOS/pull/2354)
moved preparation ahead of the destructive swap and made post-swap failures say
that the old app was down. It did not make a multi-service deployment atomic, and
some infrastructure failures still occur after teardown.

Separately, restart monitors revived tasks during stop or replacement. Fixes added
suppression, checks when queued work actually executes, and persisted group stop
intent in [#1627](https://github.com/wendylabsinc/WendyOS/pull/1627) and
[#1716](https://github.com/wendylabsinc/WendyOS/pull/1716). Native Mac redeploy needed
the same protection across supervisor, reconcile, adoption and explicit launch in
[#2220](https://github.com/wendylabsinc/WendyOS/pull/2220). NPU teardown required
cooperative owner shutdown and confirmed process exit, including checking errors
returned by the wait operation, in
[#2547](https://github.com/wendylabsinc/WendyOS/pull/2547).

Review where preparation ends and ownership changes. Check every restart entry
point, overlapping operations and rollback. Useful regressions fail preparation
while an old app runs, queue a restart before suppression, stop a group mid-restart,
and return a wait error without a confirmed exit. The NPU fix depended on companion
app cleanup and bounded device validation; it is not proof that forced termination
is unsafe for every task.

## Restart recovery must reconstruct transient resources

Container specs survived while `/run` sockets, USB tty numbers and accelerator
device grants changed. [#1675](https://github.com/wendylabsinc/WendyOS/pull/1675)
restored BlueZ proxies and resolved persisted USB identity before refreshing mounts
and device rules. [#2554](https://github.com/wendylabsinc/WendyOS/pull/2554) refreshed
NPU grants after reboot. That PR also moved duplicate mesh admission before route
publication, so a rejected connection could not disturb the incumbent route.

In [#1876](https://github.com/wendylabsinc/WendyOS/pull/1876), type-only entitlement
labels vanished because containerd deletes empty-valued labels. Runtime grants
worked initially, but restoration skipped the app after agent restart. A nonempty
marker survived persistence. Existing containers needed redeployment to acquire it.

Review the persisted representation and the actual storage library's semantics.
Separate stable identity from ephemeral paths and numbers. Test agent-only restart
with tasks still alive, reboot, and device re-enumeration. A successful fresh create
does not exercise restoration. Check that a refused new owner leaves the existing
owner's resources intact.

## Background work needs its own lifetime and ordered teardown

Audio capture inherited a unary RPC's context, so returning from the RPC killed
the recorder after roughly one chunk. EOF then looked like successful completion.
[#1945](https://github.com/wendylabsinc/WendyOS/pull/1945) assigned the capture its
own cancellable lifetime and treated unexpected producer exit as a source failure.
It also replaced a device-wide lock around slow adapter calls with per-campaign
serialization and short locks around maps.

ROS 2 exec and crash-loop logs lost their output tails because task exit triggered
pipe closure before copy goroutines drained. [#1601](https://github.com/wendylabsinc/WendyOS/pull/1601)
and [#1627](https://github.com/wendylabsinc/WendyOS/pull/1627) waited for output
completion before closing. The ROS 2 scanner also stopped reading oversized
messages, then waited forever on a writer blocked on that same pipe. Its error path
needed cancellation, draining and closure before waiting for completion.

Trace who cancels each context and who closes each pipe. Detached work still needs
an owner that stops it. Test returning from the initiating RPC, producer death,
large messages, and a process that writes its final output immediately before exit.
Inspect callbacks under locks for re-entry and slow external work. Do not add an
unbounded drain where the producer can remain alive indefinitely.

## Transport reads are not frame boundaries

The camera path rate-limited pipe reads as though each were a video frame, dropping
pieces of compressed pictures. [#2012](https://github.com/wendylabsinc/WendyOS/pull/2012)
forwarded every byte. Episode export repeated the assumption by treating indexed
reads as complete pictures. [#2452](https://github.com/wendylabsinc/WendyOS/pull/2452)
assembled access units across fragments, preserved multiple slices per picture,
and reset assembly across missing or discontinuous records.

The BLE bridge had a related representation boundary. Peripheral lengths narrowed
to signed integers before allocation and copying. [#1932](https://github.com/wendylabsinc/WendyOS/pull/1932)
bounded them before conversion, rejecting oversized indivisible GATT values while
allowing short L2CAP stream reads. At that PR's merge, the shared package had no
production consumer; its fix alone did not demonstrate end-to-end coverage.

Identify whether each layer handles bytes, messages, codec units or decoded frames.
Place rate limiting where dropping a unit preserves the contract. Test every split
position around headers, one-byte reads, several frames in one read and a missing
middle range. Check payload equality or decoded output, not just frame counts.
An intact stored stream cannot establish that no upstream data was dropped.

## Discovery can multiply work or accept obsolete results

LAN probes recursively invoked discovery when `.local` resolution failed, creating
more probes at every level. [#1674](https://github.com/wendylabsinc/WendyOS/pull/1674)
marked probe contexts and blocked that recursive browse. A worker limit per session
had not bounded the number of sessions. The reported reproduction reduced peak
memory from about 1.9 GB to 43 MB.

ROS containers independently created DDS participants and sustained reply storms.
[#2005](https://github.com/wendylabsinc/WendyOS/pull/2005) shared participants by
network namespace, interface and domain, coalesced creation, bounded per-peer
replies and released resources after the last consumer. Its device capture reported
about 611,555 versus 116 SPDP datagrams per ten seconds. In a different discovery
failure, [#2177](https://github.com/wendylabsinc/WendyOS/pull/2177) removed an outer
timeout that cut off legitimate credential attempts, waited for queued verification
and used session-wide generations to reject results from earlier device sightings.

Review the full call graph, total fan-out, sharing key and final release. A cache
key missing domain or namespace can exchange correctness for speed. Exercise failed
resolution, simultaneous consumers, withdrawal and rediscovery, and late results.
Keep an overall cancellation or scan budget when adjusting inner deadlines.

## Identity and selection must survive fallbacks and partial completion

Cloud routing IDs were mistaken for certificate identities in
[#2185](https://github.com/wendylabsinc/WendyOS/pull/2185). DNS fallback could discard
the pinned organization or endpoint and select a different same-named device in
[#2358](https://github.com/wendylabsinc/WendyOS/pull/2358). Fixes kept routing and
identity roles distinct and carried the governing pin through session selection,
certificate preparation and later retries.

Certificate refresh touched every stored auth context and spent unrelated rotating
refresh tokens. [#2375](https://github.com/wendylabsinc/WendyOS/pull/2375) made the
selected context authoritative, with explicit `--all` for broader renewal. Unenroll
had the opposite partial-completion bug: the device reset, Cloud cleanup failed,
and stale local pins survived. [#2462](https://github.com/wendylabsinc/WendyOS/pull/2462)
cleared pins and broker state after confirmed device reset even if Cloud deletion
failed. Cloud-only deletion retained pins because the device still held its identity.

Follow identity and selection through every retry, fallback and cleanup phase.
Use same-name devices in different tenants, different routing and PKI IDs, several
auth contexts, and reset-success/Cloud-failure cases. Clear trust state only on the
evidence appropriate to that transition. A connection error alone is insufficient.

## Durable state and recovery need separate failure policies

Episode and campaign writes renamed temporary files without syncing the file and
directory. Recovery could relabel completed data, ignore corrupt directories or
prevent the whole agent from starting. [#1948](https://github.com/wendylabsinc/WendyOS/pull/1948)
used the shared atomic writer, preserved completed manifests and isolated damaged
episodes. Quarantined and in-flight data counted toward quota.

Provisioning migration could overwrite state in place. An empty state file then
blocked enrollment. [#2402](https://github.com/wendylabsinc/WendyOS/pull/2402) used
synced atomic writes and distinguished empty state from malformed nonempty evidence.
On Mac, failed permission repair discarded otherwise valid enrollment in memory.
[#2447](https://github.com/wendylabsinc/WendyOS/pull/2447) retained loaded credentials
and warned when repair failed, while preserving stricter legacy migration behavior.

Review the durable commit point and distinguish unreadable authority from failed
housekeeping. Recovering one corrupt episode and recovering identity have different
trust requirements. Exercise interruption around write/sync/rename, a full or
read-only disk, one corrupt record among healthy ones and a completed manifest
awaiting directory rename. Verify that retries preserve valid state and that
quarantine cannot silently escape storage limits.

## Timestamps and loss counters can make incomplete data look valid

Late application results were acknowledged as buffered, then filed as pre-roll in
the next episode. [#1867](https://github.com/wendylabsinc/WendyOS/pull/1867) added a
bounded post-capture drain and fenced pre-roll using both agent receipt time and
presentation time. It rejected client time claims when the agent lacked a usable
boot identity and marked replayed records. The drain did not disambiguate every
late verdict arriving between episodes; that limitation remained explicit.

[#1945](https://github.com/wendylabsinc/WendyOS/pull/1945) stopped double-counting
camera sequence gaps and subscriber drops, advanced sequence tracking for deliberate
rate skips, and recorded unknown loss during reconnection as unknown.
[#1948](https://github.com/wendylabsinc/WendyOS/pull/1948) stopped comparing an absent
system-clock bound as though it were a real zero-width interval.

Trace clock domain, boot identity, receipt time, presentation time and episode
membership separately. Identify whether two counters witness the same loss. Test
late predictions, missing boot identity, unsynchronized clocks, deliberate skips
and producer reconnection. Verify the manifest's claims as well as the payload.
Do not infer zero loss, full retention or measured timing from missing evidence.

## Failure classification must preserve cause and timing

Locally canceled parallel uploads incorrectly marked a healthy broker as dead.
[#1996](https://github.com/wendylabsinc/WendyOS/pull/1996) made caller cancellation
neutral while retaining deadline and transport failure accounting. Stream cleanup
could instead manufacture a cancellation or report EOF after an observation deadline
as failure. [#2415](https://github.com/wendylabsinc/WendyOS/pull/2415) checked the
shared session before canceling child contexts and compared absolute deadlines.

[#2283](https://github.com/wendylabsinc/WendyOS/pull/2283) propagated failed starts
through the detached existing-container path before running post-start actions.
[#2084](https://github.com/wendylabsinc/WendyOS/pull/2084) preserved wrapped causes
while emitting fixed telemetry categories without raw error details.

Check the error at its origin and every wrapper, retry and final status. Test
cancellation versus timeout, EOF before a timer callback, wrapped and joined errors,
and failed starts on fast paths. Assertions should cover exit status, side effects
and preserved cause. Telemetry categories should not copy hostnames, paths or secrets.

## Capability claims need evidence for the operation being offered

[#2044](https://github.com/wendylabsinc/WendyOS/pull/2044) followed up the review of
#1909. A leftover CUDA library had advertised a backend without its driver, while
FastRPC evidence reported QNN under both GPU and NPU. The fix required driver nodes
and assigned QNN to the NPU. This affected image selection, not merely display text.

Driver OTA preflight treated unknown inventory or ambiguous target kernels too
permissively. [#1835](https://github.com/wendylabsinc/WendyOS/pull/1835) required the
relevant kernel match and kept unverified cases blocked pending explicit acceptance.
Old build hosts advertised a registry fallback that could not reach the device
over mesh. [#2229](https://github.com/wendylabsinc/WendyOS/pull/2229) refused automatic
chunked delivery early while preserving explicit registry delivery for reachable
topologies. Its old-target/new-host variant remained a separate follow-up.

Trace reported capability into the actual consumer. Check absent drivers with
libraries present, unavailable inventory RPCs, old peers and each supported network
topology. Distinguish discovery, authorization, transport reachability and readiness.
A fallback needs a reachable execution path, not merely an alternative API name.

## Flash operations must account for old state and changing device paths

Sparse writes skipped empty image ranges and preserved an old data filesystem,
including enrollment. [#2601](https://github.com/wendylabsinc/WendyOS/pull/2601)
cleared partition signature areas left unwritten by the image. This was filesystem
reset behavior, not secure erasure of all old data. Its Thor flash engine was outside
the verified scope.

After USB disconnection, provisioning retried a stale disk path.
[#2460](https://github.com/wendylabsinc/WendyOS/pull/2460) captured identity before
the failure and required a unique match after re-enumeration. It documented limits
when cloned disks share identifiers. [#2461](https://github.com/wendylabsinc/WendyOS/pull/2461)
used actual partition extents to size GPT images and relocated backup metadata,
instead of refusing a usable drive based on the source disk's declared size.

Review untouched byte ranges, filesystem recognition and device identity at retry.
Test pre-existing data, a different disk acquiring the old path, ambiguous matches,
and partitions near the target's end. Include each writer and privilege boundary
affected by the change. An image's logical size, allocated bytes and required
partition extent answer different questions.

## Representation drift breaks otherwise valid workflows

The JSON schema rejected camera credentials and the MCP entitlement that the Go
validator accepted. [#1753](https://github.com/wendylabsinc/WendyOS/pull/1753) aligned
them and added parity checks against the authoritative entitlement definitions.
[#2447](https://github.com/wendylabsinc/WendyOS/pull/2447) corrected a different
mismatch: the registry stored loopback images under `127.0.0.1`, while delivery
requested `localhost`, causing failures or reuse of stale images.

Docker probing and building could select different daemons because a clean build
configuration lost `currentContext` and a shared builder retained the previous
endpoint. [#2451](https://github.com/wendylabsinc/WendyOS/pull/2451) preserved context
selection and keyed automatic builders by it while honoring explicit overrides.

Follow a value through schema, validator, serialization, persistence and consumer.
Check normalization at both ends and include context in shared-resource keys where
it changes ownership. Useful regressions round-trip real persisted forms, compare
contract definitions and switch between two valid contexts. Test actual call-site
configuration as well as helpers. A new canonicalizer does not fix callers that
still use the old representation.
