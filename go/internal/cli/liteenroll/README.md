Lite enrollment checks CA bundle compatibility before reserving a Cloud asset or
minting its enrollment credential. Firmware defaults to Mbed TLS and accepts
RSA/ECDSA certificate algorithms. Firmware built with `CONFIG_WENDY_PKI_WOLFSSL=y`
also advertises ML-DSA-44/65/87 certificate verification. The CLI permits those
algorithms only after the selected board explicitly reports that capability.
Older firmware and an absent or false capability select the P-256 device chain.
The CLI sends `chain_algorithm=ECDSA_P256` to EST CA discovery and binds the same
choice into the operator-signed enrollment request. The authenticated discovery
response must confirm `X-Wendy-Chain-Algorithm: ECDSA_P256`, and its roots must be
self-signed P-256/SHA-256 CAs. Servers that ignore the selector cannot silently
substitute another chain. PKI issues the Class C token
for that tenant's separate P-256 device authority. ML-DSA-capable firmware retains
the normal device authority. The device continues to generate its P-256 key and
redeem the token itself; no firmware protocol change is needed.

This requires PKI support for the chain selector and broker support for the new
device authority. Older PKI deployments that return ML-DSA roots still fail before
an asset is reserved. Explicit root files and firmware-pinned roots preserve the
configured issuer instead of selecting a new authority. Firmware compatibility
checks still apply to supplied files.

Both discovered and explicit trust bundles use the negotiated compatibility
profile. Each profile still checks PEM syntax, CA status, validity, size and
certificate algorithms. ML-DSA support does not permit Ed25519 or DSA. Failed
validation leaves configuration unchanged. The fresh enrollment challenge is
checked again before minting in case the board's capabilities changed. The board
still verifies the complete issued certificate chain.

For Cloud relays, a matching P-256 device certificate whose issuer is absent from
the operator session's trust bundle triggers verified HTTPS discovery of that
tenant's P-256 device roots. The CLI then repeats full device certificate and
identity verification. A successful existing chain requires no discovery, and
peer-supplied certificates never become trust anchors.
