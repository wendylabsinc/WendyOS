# Local verified mesh directory

The private `wendy.sessionbroker.DirectoryV1/List` and `Watch` methods accept an
empty request and return version 1 `OperatorDirectory` text projections. They
carry public descriptions, not binary certificate/signature material. Topology
is independently available through binary protobuf `TopologyV1/Get`. These are
local IPC APIs, with no additional mesh publication or service record.

Production rows come from the broker's own full participant. Each joins a fresh,
verified signed device manifest to its exact authorized Babel origin, prefix,
sequence and expiry. Explicit host fixtures can inject coherent observer views.
Only supported signed WendyOS/Lite management endpoints are selectable. Cloud
inventory, physical advertisement hints and expired records do not substitute
for a verified route. Destination connections still authenticate their exact
remote TLS identity, tenant chain and profile pins.

A snapshot contains source identity, capture time, readiness and truncation.
Rows include canonical mesh selector, principal, tenant, signed name, platform,
management protocol/port, capabilities, revision/expiry and observed metric and
next hop. Metrics do not imply hop counts. Capabilities describe availability;
they do not authorize operations.

At most 256 rows and 128 KiB are encoded, including the complete source and
watcher metadata. Omitted rows set `truncated`. Up to 16 watchers share a single
one-second poll. Every watcher retains only one replaceable snapshot. A slow
consumer blocks its own sender while updates coalesce. The last watcher cancels
the pending poll, whose own deadline is three seconds. A live watcher holds an
ordinary broker activity lease; List is a single bounded observation.

Capture time and source-response-size bookkeeping are excluded from the semantic
publication hash. Device, route, manifest or readiness changes emit on the next
poll; unchanged snapshots emit a five-second freshness heartbeat within the
ten-second consumer freshness bound. A new watcher immediately receives the
latest observation. Poll/heartbeat cadence is exposed only for Watch. The source
size is the protobuf size of the locally projected topology, not network traffic.
There is no periodic neighbour topology RPC or extra signed publication here.

List, Watch and topology reads connect only to an existing matching broker.
They do not start a participant, discover radios, log in to Cloud or renew
credentials. The separately named `ReadOperatorDirectoryAutomatic` and
`WatchOperatorDirectoryAutomatic` helpers permit preparation only as part of an
explicitly requested discovery action. Public CLI/UI integrations are built on
these APIs in a separate delivery unit.

Service browsing projects inherited entries from the same manifests, filtered
by optional mesh/type, with at most 512 rows and 256 KiB. Unreachable or expired
owners disappear; unsupported optional projections are distinct from a healthy
empty list. Native application lookup filters to its exact verified target before
applying the public inventory budget, so unrelated services cannot hide a
retained target's declared port.

Positive host tests cover endpoint replacement, route loss, maximum-length
operator identities, bounded large-mesh projections, shared/coalesced polling,
watch cancellation and lease release. Hardware qualification is separate.
