# Nearby from the retained operator context

The local-only `wendy.sessionbroker.DirectoryV1/List` and `Watch` IPC methods
accept an empty message and return `OperatorDirectory` JSON byte messages,
version1. An older helper that lacks these methods is reported as unsupported;
a different version is rejected. This contract is separate from mesh CBOR and
carries no certificate material. There is no new on-mesh publication or service.

Rows are the selected operator's authenticated adjacent router observations:
only fresh verified signed device manifests joined to the exact usable,
authorized Babel origin/prefix/sequence/expiry. They reuse the normal remote
management resolver. The adjacent device may use its own locally selected,
verified origin; other devices require a remote usable route. Only supported
signed WendyOS/Lite native management protocols are exposed. Cloud inventory,
unverified physical TXT and retained expired records never substitute for a
row. This is explicitly not an independent CLI certificate/catalogue verifier.

The snapshot names its authenticated source, capture time, ready/unavailable
state, and topology/budget truncation. Each row carries its canonical selector,
full principal and tenant, signed name/platform/protocol/port/caps/revision and
expiry, plus observed metric/next hop. It never infers hop count from metric.
Unknown optional capabilities remain metadata; only recognized values such as
sensors have discovery meaning, and none authorize an operation.

Bounds: at most256rows and128KiB encoded per result, with truncated=true when
more supported rows were omitted. Up to16watchers share one one-second polling
effect. Each has a single replaceable latest snapshot, not an event history.
A slow IPC client backs up only its stream sender; the publisher coalesces its
pending snapshot. The last watcher cancels an in-flight poll, which itself has
a three-second deadline. A live watch holds the existing broker activity lease.
List is a single bounded observation. List/watch clients only connect to an
already-running matching operator broker: they never start a Session/helper,
acquire new Cloud credentials, or implicitly join a first-hop neighbour.

`discover`/`device list` default Nearby, one-shot, JSON, interactive picker and
MCP device_list consume this feed. `--type mesh` selects it alone. Physical,
simulator, provider and Cloud paths remain their existing sources. JSON includes
meshDevices and meshDiscovery metadata. The local picker and merged collection
join a physical path only by the same full canonical tenant/device principal
(or the same explicitly enrolled Lite device and tenant), retaining both paths.
Mesh loss strips only that path, and physical retraction can retain the mesh
path. Equal display names are never mesh identity keys. A mesh-only row keeps
the canonical selector as its saved default and selection target; a matching
physical path retains its normal preference. Friendly mesh-name lookup is
scoped to this selected current context and reports ambiguity before dialing.
Explicit physical addresses/provider selectors and Cloud selectors keep their
normal interpretation. Every connection still rechecks the current signed
endpoint and authenticates its exact destination TLS identity and pins.

Freshness is checked at projection and display. Expired rows are not selectable;
watch/source loss clears the mesh contribution, with physical rows retained.
An incomplete snapshot remains labeled incomplete, not a complete mesh claim.
MCP uses the feed only in local host mode, never hosted relay/device-container
mode. Lite entries are listed, while agent-only MCP connection/actions retain
their existing explicit WendyOS requirement.

Positive tests cover bounds, fresh signed endpoint changes, normal route loss/
return, coalesced slow readers, shared polling and watch cancellation/lease
release, full-principal grouping and independent path withdrawal, JSON/one-shot/
continuous listing, picker/default selection, duplicate-name ambiguity, and MCP
listing. They are host fixtures. No lab deployment or hardware claim is made.

## Unchanged-watch traffic

The shared publisher hashes its semantic projection, excluding capture time and
source-response-size bookkeeping. Unchanged polls do not emit another local IPC
payload. Device/path/protocol/capability/revision/expiry changes and source-loss
state changes emit immediately when the shared poll observes them. A five-second
full-snapshot heartbeat explicitly refreshes unchanged healthy views, within the
ten-second consumer freshness bound. A newly joining watcher receives the latest
fresh poll result immediately rather than waiting for that heartbeat.

Discovery metadata reports pollIntervalMillis/heartbeatMillis for a watch and
sourceResponseBytes, the actual protobuf application-body size of the adjacent
GetLocalMeshTopology response before optional compression/TLS framing. One-shot
List has zero polling/heartbeat cadence. The backend still performs one shared
GetLocalMeshTopology per second only while there are active watches. It has no
per-client or idle poll. This traffic is operator-to-adjacent management on the
current LAN attachment; it creates no periodic signed publication or extra mesh
gossip. Future BLE operator attachment should revisit polling versus a server
push subscription rather than assume this LAN cadence is appropriate.
