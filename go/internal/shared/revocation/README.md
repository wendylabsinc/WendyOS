# Live certificate revocation

The agent and CLI check issuer-signed full CRLs for certificates on an
authenticated peer path. A certificate that a verified CRL lists is always
refused. What happens when the CRL is missing or expired depends on what is
being decided, because an unreachable Cloud must never brick a device or
partition a site.

## Evidence levels

| Level | Used for | Missing or expired CRL |
| --- | --- | --- |
| `RevokedOnly` | Established connections, mesh carriers, device and server certificates, the CLI verifying a device | Accepted |
| `Session` | A new operator session on the agent's gRPC server, full or resumed | Accepted within the grace; refused beyond it or when there is no CRL at all |
| `Fresh` | Destructive RPCs: unprovision, OS and agent update, volume removal | Refused |

`Session` and `Fresh` only refuse when the fleet configures a grace with
`WENDY_CRL_GRACE` (a Go duration; `0` demands a current CRL). Without it, aged
or missing evidence is logged once an hour per issuer and accepted at every
level. With it, evidence is in one of three states:

- Fresh: full trust.
- Stale within the grace: sessions are admitted and logged, destructive RPCs
  return `FailedPrecondition`.
- Beyond the grace, or never seen: new operator sessions are refused.
  Connections that already exist continue.

Device certificates never fail for lack of evidence, whatever the grace:
device revocation has its own authority channel through Cloud, and this check
is defence in depth for the LAN path. A node does not judge its own identity.
Certificates without a distribution point keep their previous policy, but are
still refused when a known CRL from their issuer lists them.

## Where evidence comes from

- Fetch. The first check of an issuer registers the certificate's distribution
  URLs. A process worker refreshes them every five minutes, or halfway to
  `NextUpdate` when sooner, and retries failures after 30 seconds. A check
  never waits on the network unless it would otherwise refuse for lack of
  evidence; then it makes one bounded attempt.
- Disk. The newest verified CRL per issuer is written under `crl/` in the
  agent's config directory (`/etc/wendy-agent`) and the CLI's config directory.
  A restart starts from it, including its CRL-number floor.
- Stapling. `certs.TLSKeyPair` attaches the CRLs covering the local credential
  to `tls.Certificate.OCSPStaple`. Go sends that field with client and server
  certificates in TLS 1.3 and does not interpret it. The agent's TLS server and
  the CLI's device verifier read it from `ConnectionState.OCSPResponse`. The
  agent's gRPC TLS server does not staple its own certificate, because it also
  serves the mobile apps, whose TLS stacks may expect an OCSP response there. An
  operator machine that was online can therefore admit itself to a device that
  has no network. Stapled bytes are unverified until an authenticated path
  names their issuer and the issuer's signature checks, so a staple cannot
  vouch for its own chain, and an older staple never replaces newer evidence.
  Staples are limited to 128 KiB by the TLS certificate message; larger CRLs
  are fetched only. Clients that cannot staple, such as the mobile apps, rely
  on the device's own evidence.
- Mirror. `WENDY_CRL_MIRROR` replaces the scheme and host of certificate URLs
  and keeps their path, for self-hosted, proxied and NAT64 sites. A mirror is
  local configuration, so the public-address restriction does not apply to it.

## Validation

CRLs must have the correct issuer, a valid signature and a CRL number. Native
Go verification supports ML-DSA and classical issuers. Older CA certificates
without SKI use the same RFC 5280 key identifier derivation as PKI. A fetched
CRL with a lower number or older issue time than the known one is rejected, as
is conflicting content at the same number. This history is shared by all URLs
and CA certificate wrappers with the same issuer subject and public key, and it
persists. Delta CRLs, scoped/indirect CRLs, remove-from-CRL entries and
unsupported critical extensions are rejected.

A CRL is adopted for its revocations whatever the local clock says. Freshness
is measured against the caller's verification time, which on the agent includes
its trusted-time floor. A CRL issued after a lagging local clock is fresh, and
an expired CRL still refuses the serials it lists.

Fetches from certificate URLs accept HTTP or HTTPS on ports 80/443, use
ordinary HTTPS verification, reject redirects, ignore environment proxies, and
dial only checked public IP addresses. Resolver answers are checked before
dialing those exact addresses. Requests have a five-second deadline and four
concurrent fetch slots. Limits are four distribution URLs per certificate, 128
issuers, 4 MiB per response and 32 MiB of cached DER.

## Enforcement points

- Agent TLS: handshakes refuse a listed certificate, take the stapled CRL, and
  apply `Session` to operators when a grace is set. The gRPC server rechecks
  at unary/stream admission and stream message boundaries; those rechecks
  refuse only a listed certificate, except destructive methods.
- Mesh: device and primary-operator carriers refuse a listed certificate. The
  node rechecks every carrier's original chain each second and removes a
  revoked carrier, its Babel link and forwarding interface. A newer certificate
  for the same principal does not rescue it. Cached identities, manifests and
  route authorizations are revalidated on lookup.
- CLI, Cloud relay and Lite relay: refuse a listed certificate on classical and
  ML-DSA chains alike. Historical receipts and signatures keep their
  time-specific verification.
- Accepted CRL changes invalidate the agent's cryptographic path cache.

## Limits

- The grace is device configuration. Delivering it through signed desired
  state, and raising an alert beyond the agent log, are not implemented.
- The grace window applies to operator gRPC sessions. Mesh operator carriers
  are refused on positive revocation only.
- Staples are read on the agent's TLS server and the CLI's device verifier,
  not on mesh carrier handshakes.
- No delta CRLs and no mesh propagation of CRLs between devices.
- A non-mesh gRPC handler already executing is not canceled; the next RPC or
  message boundary refuses the revoked peer.

Tests use signed ECDSA and ML-DSA fixtures and an injected responder. No
production PKI or physical-device deployment is implied.
