# Bluetooth discovery announcements for Wendy Net

Advertisements are unauthenticated discovery hints. The service identifier is
the complete tenant UUID; this format does not conceal tenant membership from
nearby observers. TLS verifies the full tenant/device principal and resolves
equal device prefixes. Certificate-based membership in a specific named network
is future work.

## Binary Service Data, version zero

The service-data key is the tenant UUID (128 bits). Its nine-byte payload is:

| Offset | Bytes | Meaning |
| --- | --- | --- |
| 0 | 1 | Version/reserved: exactly `00` |
| 1 | 2 | LE dynamic PSM, unsigned big-endian |
| 3 | 2 | First two bytes of SHA-256 of the network-name UTF-8 bytes |
| 5 | 4 | First four canonical bytes of the device UUID |

There is no Unicode normalization or case folding of the network name. The
agent's default network name remains `tenant:<tenant-uuid>:default`. The UUID
prefix is not hashed and is not an identity or authorization token. A zero
prefix is valid, including for nonzero UUIDs whose leading bytes are zero.

Flags (3 bytes), AD framing (2), service UUID (16), and payload (9) occupy
30 bytes of the legacy 31-byte advertisement. Linux continues advertising only
Service Data; it does not add a local name or another service-UUID-list field.
The optional BlueZ `TxPower` property requests advertising-set power and is not
an instruction to include a TX-power AD field.

The former unversioned ten-byte layout is rejected. This discovery version is
independent of the Wendy Net session protocol and ALPN version.

## Local-name representation, version zero

Platforms unable to advertise Service Data can advertise the tenant UUID in
their service UUID list, plus a ten-byte ASCII local name. Treat the fields as
one unsigned 65-bit integer, most significant first:

```text
version (1 bit) | PSM (16 bits) | network hash (16 bits) | UUID prefix (32 bits)
```

The network hash and UUID prefix are interpreted as big-endian unsigned integers
for this concatenation. Encode the integer as exactly ten base-94 digits;
digit values 0 through 93 map to ASCII `!` (0x21) through `~` (0x7e). The
leftmost character is the most significant digit. Preserve leading `!`
characters. Do not encode fields separately, add a text prefix, trim, escape,
case-fold, or normalize the transmitted string. JSON/source-code escaping is
not part of the on-air representation.

Decoders reject other lengths, characters outside that alphabet, integers at
least 2^65, and any version other than zero. The one-bit slot reserves version
one; it is not accepted yet. Binary versions/reserved values other than zero
are also rejected. Both representations retain the existing LE dynamic PSM
range 0x0080 through 0x00ff.

`testdata/advertisement-v0.json` contains independent Python-generated vectors
for other implementations. For example, network `field`, PSM 0x0081, UUID
`11111111-2222-3333-4444-555555555555` produces binary
`000081c0d211111111` and local name `!&~3o8*;-z`.

Apple controls placement of the name, including scan response placement and
truncation. Ordinary background advertising omits the local name. The encoding
and Linux receiver do not constitute an iOS sender implementation or hardware
qualification. A shortened-name AD field is never accepted as a complete
payload, even when it happens to contain ten characters.

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

TLS negotiates `wendy-local-mesh-ble/1`, followed by router hello version 1.
`bleprovider.ALPN` aliases the common `localmesh.StreamALPN` constant. This
version includes shared HAVE/WANT service and presence/router announcements;
there is no full-record-push fallback. BLE advertisements keep their independent
discovery format/version. Endpoint-only leaf ALPN and hello remain separate.
