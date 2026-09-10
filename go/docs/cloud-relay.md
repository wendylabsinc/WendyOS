# PKI Cloud relay client

PKI-enrolled Go agents use `wendycloud.tunnel.v2.TunnelAuthorizationService`
and `TunnelBrokerV2Service`. Legacy enrollment continues to use
`wendycloud.v1.TunnelBrokerService`. The vendored contract comes from
service-protos commit `fe42be2`; the cryptographic domain separators remain
`wendycloud.tunnel.v1/...` by design.

The agent authenticates to Cloud's devices listener with its complete mTLS
certificate chain, obtains a signed lease, and proves possession of a distinct
P-256 signing key on the Cloud-selected relay. The relay never receives the
agent's certificate, operator token, or identity headers. The agent verifies
Cloud's JWKS signatures, issuer/audience, lifetime, key and session bindings,
and HPKE envelope before accepting an offer. Offers are persisted before
acknowledgement; relay keys and the latest lease survive process restarts.

Only the standard Cloud services `wendy-agent` (loopback port 50052) and `ssh`
(loopback port 22) are supported. Unknown services, ports, and UDP forwarding
are refused. PKI `cloud ping` measures the agent version RPC through the
`wendy-agent` tunnel, so it does not require a separate datagram catalog entry.
Device selection uses the UUID AssetService and reports enrolled offline devices.

## Endpoints and credentials

Hosted defaults:

| Purpose | Development | Production |
| --- | --- | --- |
| Device authorization / telemetry mTLS | `devices.dev.wendy.sh:443` | `devices.wendy.sh:443` |
| Grant issuer and JWKS origin | `https://api.dev.wendy.sh` | `https://api.wendy.sh` |

`WENDY_DEVICE_CLOUD_URL` overrides the device mTLS endpoint, and
`WENDY_CLOUD_GRANT_ISSUER` sets the exact trusted issuer. Keys come from that
issuer's `/.well-known/wendy-cloud-grants/jwks.json`, never an artifact-supplied
URL. `WENDY_TELEMETRY_URL` can select a separate OTLP collector; Cloud must expose
the standard logs/metrics/traces Export RPCs there. These environment variables
must be set in the agent's service environment for device-side connections.
Custom/on-prem deployments must explicitly configure their devices listener if
it differs from the enrolled Cloud address. Relay endpoints themselves come
from Cloud authorization; a PKI session cannot override them with `--broker-url`.

The new tunnel request-signature profile requires an **ML-DSA operator signing
certificate**. The current login flow issues an ECDSA certificate for TLS and
ordinary Cloud request signing; it does not obtain this second profile. Until
Cloud/PKI exposes that issuance flow to login, the CLI accepts an explicitly
issued signing pair through `WENDY_TUNNEL_SIGNING_CERT` and
`WENDY_TUNNEL_SIGNING_KEY` (PEM files). Its principal must exactly match the
logged-in operator. No EC downgrade or legacy relay fallback is attempted.

Cloud must deploy the authorization and relay services, configure grant signing
and principal attestation, and publish reachable device/relay endpoints. A
client build alone cannot enable those server capabilities.

## Verification

`go test -race ./internal/shared/cloudrelay` covers published cross-language
artifact/HPKE/proof vectors, signature and binding substitutions, certificate
signing, a full real-gRPC presence/offer/join/TCP exchange, renewal, persistence,
and cancellation. Its local test transport replaces TLS only inside the test;
production connections require TLS and validate the server name and trust chain.
