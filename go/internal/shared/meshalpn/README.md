# Known CA certificates during Wendy Net TLS setup

A fresh handshake still carries the leaf. The receiver supplies locally available
CA certificates as untrusted path candidates; its configured trust anchors and
normal certificate, principal and tenant checks remain authoritative.

ALPN variants acknowledge exact shared ancestry without a custom Go TLS fork:

```
application protocol | 00 57 43 49 01 | CA fingerprints
```

Each fingerprint is the first 16 bytes of SHA-256 over the complete certificate
DER. Fingerprints run from the root through the intermediates, excluding the
leaf. The client offers each prefix, longest first, and the ordinary application
protocol as fallback. The server selects the longest prefix it possesses. The
selected name therefore acknowledges which client CA certificates it can omit.
The server can independently omit its own CA certificates named in the client's
offers. Application dispatch uses the recognised variant's ordinary protocol.

Additional cached CAs use auxiliary, never-selected names beginning with
`wendy-net-cached` followed by a zero byte and concatenated fingerprints. A name
carries at most eight fingerprints; additional names keep inventory bounded.
Certificate fingerprints and certificates themselves stay binary on the wire.

Each name remains within TLS's 255-byte limit. Own-path prefixes contain at most
eight CAs. The Go cache retains at most 64 foreign CAs / 512 KiB, evicting in FIFO
order and reaping expired entries; an advertisement includes at most 64 extra
fingerprints. Availability makes no claim that a cached certificate is trusted.

Handshake-specific immutable snapshots retain advertised or acknowledged DER
until verification consumes it, even if the cache turns over meanwhile. No
cache mutex is held during TLS I/O or signature work. Lite uses loans from its
existing bounded RAM certificate store; the cache skips active loans and their
bytes remain alive until release, including during shutdown.
