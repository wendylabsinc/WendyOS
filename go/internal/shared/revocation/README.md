# Live certificate revocation

The agent and CLI require fresh, issuer-signed full CRLs when a certificate in
an authenticated peer path advertises a CRL distribution point. URLs come from
the signed certificate, including Wendy PKI's configured `/crl/{tenant}/{ca}`
endpoint. No Cloud login token or enrollment credential is sent. Locally
configured trust anchors remain local trust decisions.

The first use of an issuer/URL fetches its CRL. Process lifecycle workers refresh
registered endpoints every five minutes, or halfway to `NextUpdate` when that is
sooner. Failed requests retry after 30 seconds. Refresh continues without peer
traffic. Concurrent cold requests share one fetch. Background I/O does not block
reads of an existing CRL.

CRLs must have the correct issuer, a valid signature, a CRL number, and a current
`ThisUpdate`/`NextUpdate` interval. Native Go verification supports ML-DSA and
classical issuers. Older CA certificates without SKI use the same RFC 5280 key
identifier derivation as PKI. Lower CRL numbers, older issuance timestamps and
conflicting content at the same number are rejected. This history is shared by
all URLs and CA certificate wrappers with the same issuer subject and public key. Delta CRLs, scoped/indirect
CRLs, remove-from-CRL entries and unsupported critical extensions are rejected.

Fetches accept HTTP or HTTPS on ports 80/443, use ordinary HTTPS verification,
reject redirects, ignore environment proxies, and dial only checked public IP
addresses. Resolver answers are checked before dialing those exact addresses.
Requests have a five-second deadline and four concurrent fetch slots. Limits
are four distribution URLs per certificate, 128 issuer/URL entries, 4 MiB per
response and 32 MiB of cached DER. Parsed lists and serial indexes add bounded
memory overhead. HTTP cache revalidation is requested on every fetch.

## Enforcement

- CLI device verification checks CRLs on full and resumed TLS handshakes.
- Agent TLS checks include resumed sessions. Its gRPC server checks again at
  unary/stream admission and stream message boundaries on existing connections.
- Device and primary-operator mesh credentials use the same policy across LAN,
  QUIC, stream/BLE, and operator router carriers.
- Accepted CRL changes invalidate the agent's cryptographic path cache. Cached
  path authorization cannot outlive CRL `NextUpdate`.
- The mesh node checks its own identity and every carrier's original certificate
  chain every second. Revoked peers lose their carrier, Babel link and forwarding
  interface. Local credential failure stops the node. A newer certificate for
  the same principal cannot make an old revoked carrier valid.
- Cached identities, manifests and signed route authorizations are revalidated
  on lookup. Revocation therefore applies beyond direct neighbors.
- Cloud relay and Lite relay live verifiers use the same checks. Historical
  receipts and signatures retain their existing time-specific verification.

## Offline behavior and draft limitations

A fresh cached CRL permits operation during an endpoint outage until
`NextUpdate`. Known revoked serials remain rejected even when that evidence
expires or a later fetch fails. Missing, stale or invalid evidence fails closed
for certificates advertising a distribution point. There is no silent soft-fail
mode. Certificates without distribution points retain existing verification and
lifetime rules; this change does not invent an endpoint for those certificates.

The cache and rollback protection are process-local. Restart requires a new
fetch. This draft does not provide durable CRL storage, offline stapling, mesh
propagation of signed CRLs, delta merging, or push notifications. These are
separate requirements for deployments that must start without Internet access.
The node needs a sufficiently accurate clock to establish CRL freshness.

Revocation latency includes PKI publication and the refresh interval, then at
most the mesh maintenance interval under normal scheduling. A non-mesh gRPC
handler already executing is not asynchronously canceled; later RPC/message
boundaries reject the revoked peer. Existing ordinary CLI streams are not
proactively closed by the client. Active mesh carriers are explicitly removed.

Tests use signed ECDSA and ML-DSA fixtures and an injected responder. They cover
refresh, invalid evidence, stale/offline operation, rollback, request bounds,
concurrency, TLS resumption, existing RPC checks, warm caches, and device/operator
mesh carrier removal. No production PKI or physical-device deployment is implied.
