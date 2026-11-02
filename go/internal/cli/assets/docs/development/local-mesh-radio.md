# Wendy Lite and Wendy Net radio protocols

The original endpoint-only Lite protocol and its `leafBLEPeers`, `leafListen`
and `leafTimeURL` host configuration have been retired. Those fields are rejected
by the current configuration loader; remove them when upgrading.

The replacement uses ordinary authenticated router streams and complete signed
v4 device manifests. Native Lite discovery, radio admission and listener support
are integrated with the matching firmware and WendyOS carrier runtime. This
protocol-layer change alone does not activate that runtime.

## Operator routers

The operator admission layer lets a CLI installation participate under its
organization's operator certificate instead of enrolling the computer as a
device. Each installation has a persistent, independent router identity and
address. Multiple computers or operator profiles can therefore participate
without sharing routing state or changing an enrolled device's identity.
The carrier and destination still authenticate the selected tenant and exact
operator credentials. CLI broker startup and automatic carrier discovery are
separate integration steps; adding this admission layer alone does not start a
CLI router.

On supported WendyOS nodes, incoming operator connections over bluetooth use
one separate admission slot rather than a device-neighbor slot. This requires
the node's primary operator trust anchors and still shares the physical
adapter's bandwidth and connection limits.

## Version compatibility

Use matching operator-capable agents and clients. An older agent can reject
persisted operator routing receipts with `invalid directory receipt`, even
when its kernel routing cleanup succeeds. Downgrading after operator mesh use
is therefore not yet qualified. Do not delete routing history or re-enroll as
a routine downgrade workaround: that does not preserve the original identity's
replay protection.

For existing WendyOS Bluetooth links, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).
