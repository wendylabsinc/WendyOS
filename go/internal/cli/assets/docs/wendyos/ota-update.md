# WendyOS OTA update protocol

`GetOSUpdateStatus` (agent gRPC API) returns the persisted update record plus,
when requested, a live snapshot of the `wendyos-update` engine
(`OSUpdateEngineStatus`, both v1 and v2 of the agent proto).

### `diagnostics` (field 6)

A `map<string, string>` of raw, display-only connector diagnostics emitted by
`wendyos-update status --json --verbose`. Keys and values are connector-specific
and additive:

| Connector | Example keys |
|---|---|
| `tegrauefi` | `RootfsStatusSlotA`, `RootfsStatusSlotB`, EFI boot-chain and capsule variable names |
| `ubootenv` | uboot environment variable names |

The field is absent when the caller did not request engine status
(`include_engine_status: false`). Consumers must treat unknown keys as
informational and must not rely on key presence for correctness.
