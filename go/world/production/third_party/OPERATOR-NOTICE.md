# Wendy operator credential adapter

The Wendy credential schemas, named-context selection, keychain read protocol
and certificate presentation behavior were inspected at the public revision
recorded in OPERATOR-SOURCES.json. That manifest records original
Git blobs, source hashes and private adaptation hashes. The upstream Apache-2.0
license is retained in WENDY-LICENSE; preserve it and this notice
when redistributing adapted portions. This notice does not license private
World code for publication.

Changes from the referenced source:
- resolver.go selects exact auth/name/certificates/CertificateInfo fields. It
  requires explicit path/context/tenant/operator selection, refuses ambiguous
  contexts or rotation, and excludes defaults, migrations, login, token loading,
  enrollment, renewal and all configuration writes.
- Certificate preparation follows TLSKeyPair's issuer ordering, identical-DER
  deduplication and omission of proven self-signed CA roots. Strict PEM parsing
  and counts/byte limits reject skipped input. Existing upstreamcerts performs
  Wendy ASN.1 normalization; its extraction has a separate source inventory.
- keychain.go adapts only the fixed macOS generic-password read, keychain:v1 key
  references, standard-base64 decoding and five-second timeout. It adds output
  bounds, account validation, cancellation and suppressed subprocess errors.
  Writes, deletion, unlock, migration, probes and caches are excluded.
- The Cloud signer was reviewed for custody semantics. Its audience, key ID,
  enrollment and generic signer were not copied. The private signer accepts
  only typed World reads and explicit short-lived self-only read commissioning.
- files.go, sign.go, CLI validation and tests are new private code. Explicit
  trust and authenticated time precede custody access; key matching, config
  stability and time are checked before output.

No dependency was added. Existing notices cover x/sys, google/uuid and the
certificate verifier. Tests generate untrusted identities and inject fake
custody/time; they do not access installed configuration or the system keychain.
