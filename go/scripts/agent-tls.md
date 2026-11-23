# Agent TLS build correction

Build agents with `make -C go build-agent` (or a `build-agent-linux-*` target).
For custom flags, use `python3 go/scripts/with-agent-tls.py build ...` instead
of a bare `go build`. The release workflow uses the same wrapper.

Go 1.27.4's [middlebox mitigation](https://go.dev/issue/82536) removes ML-DSA
from `signature_algorithms_cert`, including the server's CertificateRequest.
Wendy operator certificates use ML-DSA. OpenSSL obeys that request, sends no
certificate, and receives `certificate_required`; Go CLI access can still work.

The build overlay changes only the TLS 1.3 **server CertificateRequest**:
it adds any supported handshake signature algorithms missing from the
certificate-signature list. Existing certificate algorithms stay present,
including RSA PKCS#1 signatures. The upstream ClientHello mitigation stays
intact. The normal Go version/FIPS filters still determine the supported list;
certificate-chain and identity verification are not changed.

This is a scoped correction to the bundled Go TLS server, not a toolchain
downgrade or a change to the console's certificate-selection rules. No file in
the installed Go toolchain is edited. The wrapper rejects other toolchain
versions and unexpected source: on an upgrade, review the upstream behavior
and remove or update this correction rather than silently shipping it.

Run `python3 .github/scripts/test-agent-tls.py` with Python linked against
OpenSSL 3.5+ (the hosted console's minimum). It compares stock and corrected
builds, inspects the decrypted certificate request, exercises authenticated
ML-DSA and ECDSA clients, and checks that absent/untrusted credentials fail.
