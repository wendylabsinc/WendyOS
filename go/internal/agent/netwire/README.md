# Wendy Net binary protocol

This package supplies the canonical CBOR codec, bounded extensible-artifact
decoder, SHA-256 text/byte conversion and semantic field-key lookup. It is a
standalone foundation: it does not change existing transports or implement the
manifest, carrier, Lite, or WASM integrations described below. Those layers must
adopt this wire contract together.

`Marshal` emits canonical CBOR for ordinary Go values. Schema structs must declare
their compact names with `cbor` tags, and dynamically built maps must call `Key`
explicitly; the codec does not rewrite map keys or text values. `Unmarshal` permits
unknown fields, while `UnmarshalSigned` rejects unknown struct fields. Nil Go
slices and maps encode as empty containers, including an empty byte string for
`[]byte`, rather than CBOR null. Use `omitempty` for optional fields that should
be absent.
`UnmarshalExtensibleArtifact` also validates skipped metadata against the bounded
artifact value grammar. Callers enforce their schema's required fields, types,
null restrictions and version rules, and supply message-specific byte bounds.
Extensible artifacts require nonempty input and a positive `maxBytes` limit.
`RawMessage` embeds its existing encoding verbatim, including in a canonical
envelope, so callers must retain and validate foreign signed bytes rather than
expecting `Marshal` to normalize them.

This prototype format is a breaking change. Update peers together: there is no
JSON/base64 fallback, old service-catalogue protocol or compatibility decoder.
Carrier ALPNs remain `wendy-local-mesh-ble/2`, `wendy-net-endpoint/2` and
`wendy-net-router/3`. Application TLS is independent of carrier TLS.

The wire uses definite CBOR maps, arrays, text, raw byte strings, integers and
booleans. Defined map fields have single-letter names; no positional tuples.
Go/JSON field names remain descriptive. `keys.go` records semantic field mappings;
Lite's `wn_key_name` uses the same mapping. Letters may be reused in different
maps. Text values, user TXT values and opaque signed bodies are never rewritten.
Certificates travel as DER, and hashes/signatures/payloads are byte strings.

The reliable neighbour stream retains a 4 byte big-endian payload length and a
64 KiB frame bound. There is no secondary catalogue framing or service channel.
HAVE/WANT/DATA/UNAVAILABLE/GONE transfer complete immutable manifests. GONE
promptly retires a supplier. Certificate descriptors, bounded DER chunks, signed
time and operator authorization use the same short-key CBOR convention.

The signed device envelope has `p` principal, `b` embedded body and `s` signature.
Its body contains:

| Key | Meaning |
| --- | --- |
| `v` | Device manifest schema version, currently 4 |
| `d` | Device UUID |
| `R` | Monotonic device revision |
| `i`, `e` | Issued time and the single expiry, in Unix milliseconds |
| `n`, `l`, `c` | Display name, platform and capabilities |
| `P`, `T` | Native agent port and transport |
| `a` | App TLS/QUIC endpoints |
| `r` | Exact authorized Babel origin: `p` prefix, `r` router ID, `s` sequence |
| `s` | Complete active service list |
| `q`, `w` | Required extensions and withdrawal, when present |

Each service map contains `m` mesh, `a` app ID, `s` service ID, `t` DNS-SD type,
`n` instance, `p` port and optional `x` TXT strings. It inherits device identity,
revision and lease from the enclosing manifest. Omission removes a service.
There are no service signatures, generation journals, inventories, freshness
proofs or separately gossiped/cached service blobs. Remote lookup metadata is a
projection of an already verified manifest, not independent authority.

All active services must fit in one manifest: at most 256 entries and a 32 KiB
body, including metadata. Local admission reserves 2 KiB for non-service fields
and rejects oversized replacements without truncating the authoritative list.
The maximum lease is 15 minutes, clipped to certificate validity, with a quiet
refresh every 5 minutes. A first change publishes immediately; subsequent changes
coalesce until one second after the previous publication. DNS TTL refreshes only
maintain local liveness and never renew the signed record. Service changes do not
advance the Babel sequence number.

Signed device bodies use deterministic CBOR encoding. The signature domain is
`wendy-local-mesh/manifest/4` followed by NUL, then the principal length, principal
and exact original body. Raw bytes survive verification, forwarding and caching;
never reconstruct a foreign signed body from its parsed projection. Known typed
fields cannot be null. Unknown optional values remain structurally bounded and
skippable, and unknown control kinds do not close a healthy carrier.

Bounds remain depth 16, 64 map pairs and 1,024 array elements. Lite's parser uses
12 byte tokens and retains the services array as a span, validating/iterating
one child map at a time. FIFO payload retirement and active-upload pins apply to
the full manifest. C6 may discard the full blob after verification and retain
only bounded endpoint/service lookup metadata; C5 retains full manifests in PSRAM.

The local WASM publication/browse ABI still uses JSON. The host converts a guest
definition once to compact binary intent and renders verified lookup metadata
back to JSON. Cloud HTTP, management protobufs and host JSON diagnostics retain
their existing formats; none is a mesh wire fallback.

`HashText` converts exactly 32 digest bytes to lowercase hexadecimal; `HashBytes`
accepts exactly 64 lowercase hex characters and returns those 32 bytes. Both
return an error for other lengths, and `HashBytes` also rejects invalid or
uppercase hexadecimal. These helpers convert identifiers; they do not hash input.
