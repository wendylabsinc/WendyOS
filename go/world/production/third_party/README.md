# Pinned Wendy certificate policy extraction

This private module contains a limited extraction from the public Wendy source
at the revision recorded in `SOURCES.json`. Original files were read with
`git show` from that object, not copied from a potentially modified working tree.
The manifest records original Git blob IDs and SHA-256 digests, plus hashes of
the adapted files. The upstream `WENDY-LICENSE` is Apache-2.0. CIRCL, UUID, and
x/sys license notices accompany their pinned dependency versions. Retain these
notices and the source/change inventory when distributing this module.

## Extraction boundaries and changes

| Private file | Pinned public source | Changes |
| --- | --- | --- |
| `internal/upstreamcerts/orgident.go` | `go/internal/shared/certs/orgident.go` | Package declaration and provenance comment only. Legacy identity parsers remain for faithful extraction; the public wrapper admits canonical operators only. |
| `internal/upstreamcerts/issuer_failure.go` | `go/internal/shared/certs/issuer_failure.go` | Package declaration and provenance comment only. |
| `internal/upstreamcerts/peer_chain.go` | `go/internal/shared/certs/peer_chain.go` | Omitted `TLSKeyPair` and its unused imports. Retained chain verification, expiry, EKU, CA constraint and issuer checks. |
| `internal/upstreamcerts/mldsa.go` | `go/internal/shared/certs/mldsa.go` | Retained ASN.1 types, certificate PEM helpers, ML-DSA algorithms and signature verification. Omitted device TLS client setup, pins, and their types. No private-key lookup, parsing, generation or issuance is included. |
| Extracted `*_test.go` | Same basename in `go/internal/shared/certs` | Package declaration and provenance comment only. They cover identity parsing, EKU handling and issuer-failure classification. |
| `internal/upstreamcerts/peer_policy.go` | Policy adapted from `go/internal/agent/mtls/mldsa_verify.go` | Uncached strict peer leaf profile and standard-x509-first/ML-DSA-fallback path. Replaced wall-clock/floor adjustment with an explicit trusted interval. No logging, network fetching or TLS session cache. |
| `wendycert/verifier.go` | Private wrapper; operator gate follows `go/internal/shared/meshoperator/identity.go`; raw signature semantics follow `go/internal/agent/services/postbox.go` | Bounded DER inputs and configured anchors; canonical operator-only and ML-DSA-65 signing-key admission; canonical parsed-key hash identity; no role grants or credential access. |

`peer_policy.go` retains the upstream maximum certificate lifetime of 732 days,
rejects CA leaves, requires explicit clientAuth, and never treats a peer's
certificate as a trust anchor. Configuration rejects any non-CA anchor rather
than silently discarding it. Certificate inputs use strict DER, as Wendy's
mesh peer verification does. Existing PEM normalization helpers remain internal;
the public API does not guess PEM encodings and preserves upstream parser behavior.

The interval adapter filters every candidate issuer and configured anchor to
certificates valid for the entire closed `[earliest, latest]` interval before
selecting a path. It does not combine paths valid at opposite endpoints.
Standard verification returns the best successful path's minimum expiry;
the supplementary verifier returns its successful path's minimum expiry.
Unused issuers and roots do not shorten `Identity.ValidUntil`.
`Identity.ValidFrom` conservatively retains the supplied earliest bound.
Neither value establishes continuing revocation or site permission.

The public wrapper caps the presented chain and configured anchor set separately
at eight certificates and 64 KiB each. It copies DER before parsing and retains
no caller-owned certificate buffers. Identity digests use lowercase hexadecimal
SHA-256 over exact certificate DER and canonical public-key SPKI bytes from
`x509.MarshalPKIXPublicKey(cert.PublicKey)`. Key identity survives same-key
certificate renewal; the certificate fingerprint changes. Go 1.27.3 rejects
ML-DSA algorithm parameters, including NULL, but accepts an extra element after
the SPKI BIT STRING. A regression re-signs that alternate encoding with a test
issuer and verifies identical key identity and candidate key sets. Hashing raw
SPKI would allow alternate encodings to change replay and key-revocation
identity. Every candidate must have a marshalable parsed public key; otherwise
the wrapper fails closed, including when that candidate is unused. This does
not change the extracted upstream certificate-policy code. Request signatures
use raw ML-DSA-65, empty context, and no added prehash. This is distinct from
WendyOS release-artifact signing.

## Conservative CA revocation

`CandidateCertificateSHA256()` and `CandidateSigningKeySHA256()` return copies of
sorted, deduplicated fingerprint sets covering all presented certificates and
all configured anchors. They describe candidates, not the selected path.
Admission must check both sets against its current revocation snapshot, in
addition to principal/policy revocation. A revoked unused candidate therefore
denies an otherwise valid request. A revoked configured CA can require a
commissioned trust-bundle update even if another valid path exists. This
conservative behavior closes CA-revocation bypass without changing upstream
path selection. Exact-path availability improvements are separate future work.

## Test-only identities and runtime limits

`internal/testidentity` creates ephemeral, deliberately untrusted ML-DSA test
roots, intermediates and operators with Go's certificate implementation. It
does not read credentials, contact an issuer, save keys or install trust. Only
tests may import it. It is not production CA or credential issuance.

Tests cover genuine ML-DSA chains and signatures, native and forced fallback
paths, peer leaf profiles, foreign/device/service/signer identities, malformed
or over-budget chains, unknown roots, expiry and constraints, full-interval
path selection, same-key renewal and owned revocation-candidate slices.
Production interoperability with issued Wendy credentials remains unverified.
The package authenticates a certificate proof only. Local site policy,
revocation freshness, trusted time and durable replay are required separately.
It exposes no network route, signing custody, physical mode or task authority.

Use the explicit `GOTOOLCHAIN=go1.27.3` toolchain for this separate module.
The parent simulation module and its trust/clock guards remain unchanged.

## Time and credential adapters

The authenticated-time extraction has its own [source inventory](TIME-SOURCES.json)
and [notice](TIME-NOTICE.md). It omits upstream clock mutation, provisioning and
multicast trust. Its private verifier also rejects expired delegated keys whose
validity ends at zero.

The operator helper records its separate credential-format and custody references
in [OPERATOR-SOURCES.json](OPERATOR-SOURCES.json) and
[the operator notice](OPERATOR-NOTICE.md). It reads an explicitly selected existing
credential and signs only bounded World read/control formats. It does not copy
Wendy's login, issuance, automatic renewal or configuration migration behavior.
