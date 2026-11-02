# Wendy Lite and Wendy Net radio protocols

The original endpoint-only Lite protocol and its `leafBLEPeers`, `leafListen`
and `leafTimeURL` host configuration have been retired. Those fields are rejected
by the current configuration loader; remove them when upgrading.

The replacement uses ordinary authenticated router streams and complete signed
v4 device manifests. Native Lite discovery, radio admission and listener support
are integrated with the matching firmware and WendyOS carrier runtime. This
protocol-layer change alone does not activate that runtime.

## Operator routers

An operator router uses your organization's operator certificate rather than an
enrolled device identity. Each CLI installation has its own persistent router
identity and address, so multiple computers can participate independently.
Connections authenticate both the first router and the destination against the
selected organization and operator credentials. The CLI also needs router startup
and discovery support; agent-side admission alone does not start it.

On supported WendyOS nodes, an incoming operator connection over Bluetooth uses
one separate slot, in addition to the device-neighbour slots. The node must trust
your organization's primary operator certificates. All connections still share
the adapter's bandwidth and hardware connection limits.

## Version compatibility

Use matching operator-capable agents and clients. An older agent can reject
persisted operator routing receipts with `invalid directory receipt`, even
when its kernel routing cleanup succeeds. Downgrading after operator mesh use
is therefore not yet qualified. Do not delete routing history or re-enroll as
a routine downgrade workaround: that does not preserve the original identity's
replay protection.

For existing WendyOS Bluetooth links, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).
