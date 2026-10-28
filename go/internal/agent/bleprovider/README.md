# Bluetooth discovery announcements for Wendy Net

Advertisements are unauthenticated discovery hints. The service identifier is
the complete tenant UUID; this format does not conceal tenant membership from
nearby observers. TLS verifies the full tenant/device principal and resolves
equal device prefixes. Certificate-based membership in a specific named network
is future work.

## Binary Service Data, version one

The service-data key is the tenant UUID (128 bits). Its ten-byte payload is:

| Offset | Bytes | Meaning |
| --- | ---: | --- |
| 0 | 1 | Version: exactly `01` |
| 1 | 1 | Capabilities: bit0 native carrier admission open; bit1 encrypted time relay available |
| 2 | 2 | Native LE dynamic PSM, unsigned big-endian |
| 4 | 2 | First two bytes of SHA-256 of the network-name UTF-8 bytes |
| 6 | 4 | First four canonical bytes of the device UUID |

Unknown capability bits are preserved but ignored for selection. The native PSM
remains the carrier endpoint; it is never repurposed to mean busy. The encrypted
time relay is a separate endpoint at reserved PSM130, advertised only while
its existing bounded admission has room. A full node can
advertise `02` while retaining time discovery; a node offering both advertises
`03`. Native candidate selection requires bit0. This is advisory availability,
not a reservation, guarantee, identity or route authorization. Established links
are unaffected by later advertisements.

The agent's default network name is `tenant:<tenant-uuid>:default`; no Unicode
normalization/case folding occurs. The device prefix is a short hint, including
valid zero prefixes. TLS still checks the complete principal and tenant.

Flags (3 bytes), AD framing (2), service UUID (16), and payload (10) occupy
exactly31 bytes. Linux emits only Service Data, without a local name or redundant
UUID-list field. Optional BlueZ TxPower config does not add a TX-power AD field.
Version zero and the old unversioned format are intentionally unsupported.
Discovery version is independent of the session ALPN version.

Incoming capacity includes pending reservations. The existing scan-loop cadence
coalesces changes onto the existing advertisement refresh worker; the separate
time listener stays advertised when the carrier is full. On reception, a live
busy observation immediately replaces cached ready state. Selection and the
queued dial recheck require the newest fresh ready observation; skipped busy
candidates incur no dial failure penalty. Already opened/established links are
not canceled by an availability hint.

## Local-name representation, version one

The receiver supports platforms that publish the tenant UUID plus a complete
local name. There is currently **no production local-name sender** in this
repository. Linux advertises binary Service Data. We retain the receive fallback
without a second packed field layout: interpret the exact canonical ten binary
bytes as one unsigned80-bit big-endian integer and encode it as exactly thirteen
base94 digits, using ASCII `!`..`~`, most significant digit first. Preserve leading
`!` characters. Decoding reconstructs the same ten bytes and applies the same
version, capabilities, native PSM, network and device-hint semantics.

This deliberately replaces the old ten-character representation. Thirteen name
characters fit in a legacy scan response; complete names may be correlated with
the tenant UUID in the primary advertisement. `testdata/advertisement-v1.json`
contains independent Python-produced binary and name vectors. No speculative
65-bit capability packing or iOS sender implementation is introduced.

Apple controls placement of the name, including scan response placement and
truncation. Ordinary background advertising omits the local name. The encoding
and Linux receiver do not constitute an iOS sender implementation or hardware
qualification. A shortened-name AD field is never accepted as a complete
payload, even when it happens to contain thirteen characters.

## Linux reception and freshness

BlueZ owns scanning, its governor/recovery policy, and advertisement instances.
Existing BlueZ Service Data signals remain usable for binary discovery. A
receive-only Linux management socket observes live Device Found events from
the selected adapter and supplies advertising and scan-response AD fields.
It never sends management commands or starts a second discovery session.
The management socket normally requires CAP_NET_ADMIN.
The receiver checks that capability explicitly: an untrusted management socket
can bind successfully while being ineligible to receive Device Found events.

BlueZ's DuplicateData option promises repeated ServiceData/ManufacturerData
signals, not repeated unchanged names. Consequently, Device1 Name, Alias,
UUIDs, RSSI, object creation, and polling are never freshness evidence for
local-name announcements.

The receiver accepts binary data from the matching tenant Service Data field.
For text data it requires a live tenant service UUID list and a complete valid
local name, either in one report or correlated within three seconds for the
same adapter, radio address, and address type. The older component timestamps
the candidate, which expires after fifteen seconds. A repeated name cannot
renew an old tenant UUID, and a repeated UUID cannot renew an old name.
The kernel can concatenate primary advertising and scan-response AD streams.
A consistent shortened/complete name pair is resolved to the complete name,
including when padding separates the streams. A shortened name in a separate
primary report supplies no payload, but its tenant UUID can be correlated with
a later complete scan response. Conflicting names are rejected.

Valid binary advertisements take precedence while fresh. Malformed or unknown
binary data for our service invalidates the observation without falling back
to text in that report. Unrelated services cannot refresh the Wendy Net observation.
State is bounded to 256 complete and 256 partial observations. Live complete
observations remain in place when full; new radio addresses wait until older
observations expire. Current BlueZ
device addresses/types must still match the live text report before dialing.

Device removal, source failure, and discovery transitions clear local-name
correlation as appropriate. Source failures retain binary reception and
established streams, report degraded discovery health, and retry every five
seconds. Recovery of scanning does not clear an independent report-source
failure, or vice versa. Only authenticated full principals enter peer identity
and radio-selection state.

## Connection scheduling

The provider uses the shared `localmesh.ConnectionPolicy` and `CandidateQueue`:
2 outgoing connections, 1 outgoing setup at a time, 3 total connections. Live
candidates of either UUID order participate in shuffled selection. Only actual
failures receive a retry penalty; capacity/pacing skips do not. An active
incoming handshake from the same radio address suppresses an outgoing attempt
while its socket I/O is progressing, with a five-second idle escape.

TLS validates the full peer principal without an ordering restriction. Both
endpoints use higher-UUID-initiates solely to retire a crossed duplicate after
the mesh hello succeeds. Closing the losing TLS stream closes only its CoC;
no `Device1.Disconnect`, management disconnect or ACL abort is issued. The
provider aliases `localmesh.StreamALPN` so TLS and stream admission cannot drift.

## Router-hop protocol

TLS negotiates `wendy-local-mesh-ble/2`, followed by router hello version 1.
`bleprovider.ALPN` aliases the common `localmesh.StreamALPN` constant. This
version includes shared HAVE/WANT service and presence/router announcements;
there is no full-record-push fallback. BLE advertisements keep their independent
discovery format/version. Endpoint-only leaf ALPN and hello remain separate.

## Certificate issuers

Enrolled ML-DSA issuers are about 20 KB per direction, five times the 3.9 KB
leaf. At the ~1 KB/s a busy CoC carries, two full chains (about 50 KB) need
more than the 30-second inbound handshake budget. A client whose presented
issuers are all its own trust anchors also offers the ALPN value
`wendy-net-issuers/<digest>`, where the digest is the first 16 bytes of SHA-256
over the sorted SHA-256 of each issuer, in unpadded base64url. Servers never
select it. A server whose own issuers have the same digest presents only its
leaf. The client then presents only its leaf if the server's leaf names the
client's issuer. Every other pair exchanges full chains, so a peer without this
value interoperates unchanged. Each side still verifies the received leaf
against its own trust anchors: an issuer the receiver lacks fails verification,
it is never skipped. The digest travels in the clear ClientHello and names only
the issuer set; advertisements already carry the tenant service UUID.
