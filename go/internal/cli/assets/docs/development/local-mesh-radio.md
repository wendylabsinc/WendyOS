# Wendy Lite and Wendy Net radio protocols

The original endpoint-only Lite protocol and its `leafBLEPeers`, `leafListen`
and `leafTimeURL` host configuration have been retired. Those fields are rejected
by the current configuration loader; remove them when upgrading.

The replacement uses ordinary authenticated router streams and complete signed
v4 device manifests. Native Lite discovery, radio admission and listener support
are integrated with the matching firmware and WendyOS carrier runtime. This
protocol-layer change alone does not activate that runtime.

For existing WendyOS Bluetooth links, see
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).
