# Delegated operator access

MCP operator credentials retain the enabling user's operator SPIFFE identity and
use a separate key. The certificate's critical FleetScope extension limits which
devices and applications the delegated key can reach. PKI records the certificate fingerprint, owner and MCP delegation ID.

FleetScope uses OID `1.3.6.1.4.1.65441.1.4`. Its value is a DER sequence with these
fields, in order:

1. Version integer, currently `1`.
2. Delegation ID, a canonical UUID encoded as UTF8String.
3. Owner principal, UTF8String matching the leaf's operator SPIFFE SAN exactly.
4. A sequence of UTF8String device principals in the owner's tenant.
5. A sequence of UTF8String application IDs. An empty sequence grants no app access.

Device principals are exact matches. Application IDs match
`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. There are no wildcards. Duplicate entries,
unknown versions, trailing DER, noncritical scopes and malformed constraints are
rejected. Each extension is limited to 16 KiB and each list to 128 entries.

The certificate must also carry the existing entitlements extension,
`1.3.6.1.4.1.65441.1.1`, with an ASN.1 sequence of UTF8String values of the form
`entitlement:<fully-qualified-service>:<RPC-method>:allow` or `:deny`.
A matching deny overrides allow. Delegated credentials do not accept wildcard
entitlements. Missing method authority denies the request.

The first reviewed methods are v2 device information and hardware capabilities,
v1 hardware capabilities, and v1/v2 container start and stop. Container requests
must name an allowed `app_name`. Every other method is denied, including shell,
deployment, tunnels and unfiltered list APIs, even when named in an entitlement.
Adding a method requires reviewing its effects and defining its resource check.

Mandatory mTLS gRPC interceptors check each unary request and each received stream
message before it reaches the service. Streaming output cannot precede request
validation. The device identity comes from its own certificate, never a request
field. Ordinary operator credentials keep their existing behavior.

Only the gRPC server with these interceptors acknowledges the critical extension.
Generic TLS configurations, including the registry and BLE endpoints, reject it.
Older agents reject the unknown critical extension. This avoids granting the
ordinary user's full authority when an endpoint cannot enforce the delegation.

These certificate constraints are an upper bound, not a live permission lookup.
Cloud must intersect them with the user's current permissions and active MCP grant
on every tool request. Disabling a grant must revoke the certificate through the
revocation service. The mTLS server checks signed full CRLs on handshakes, resumed sessions and RPCs,
and polls active streams every 30 seconds. Unavailable or stale evidence denies access.
This requires issuer publication infrastructure and does not turn an old but unexpired
CRL into evidence of a newly revoked certificate.
The backend must never retain an upstream user OAuth access or refresh token that
could enroll an unrestricted replacement certificate. Issuance and reissuance need
a fresh browser-authorized Cloud grant carrying these exact constraints.

This enforcement is local and requires no SaaS connection. A self-hosted issuer
can issue the same signed constraints under its configured trusted CA.

The device implementation recognizes the Wendy production OIDs above. Self-hosted
PKI configuration must use the same OIDs for interoperable delegated credentials;
custom OID assignments are not accepted by this implementation.

## Hosted gateway status

The gateway authenticates GET discovery with its configured machine's DPoP token
and a proof bound to the GET URL. This path no longer enrolls a machine certificate.
CLI enable/disable settings carry an ML-DSA-65 operator signature over the exact JSON
body, organization, HTTP operation, audience, time window and nonce; enabling also
requires an explicit service-account subject. Cloud verifies signer ownership and
persists the approval with the settings.

Hosted device and raw-service connections currently fail closed. They cannot use
the service-account certificate as a fallback. Per-user issuance and tunnel-principal
integration, including preventing unrestricted machine enrollment, remain WDY-3526.
These changes do not enable the hosted service or approve bearer-token exceptions.
