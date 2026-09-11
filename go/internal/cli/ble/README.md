# `internal/cli/ble` — Wendy protocol clients over BLE

The Wendy-specific layer on top of the generic BLE central in
[`internal/shared/ble`](../../shared/ble/). Everything here knows about Wendy: service UUIDs, L2CAP
PSMs, command and status bytes, protobuf framing, the Wendy PKI. Nothing in `shared/ble/central` or
`shared/ble/scan` does, and keeping it that way is the point of the split — see that package's
README.

| File | Role |
| --- | --- |
| `agent_client.go` | `AgentClient` — WendyOS agent RPC over mTLS-over-L2CAP. Uint16-BE length-prefixed protobuf frames, `agentpb` types, Wi-Fi / apps / hardware / version commands. Owns `DefaultL2CAPPSM`, the PSM the agent listens on. |
| `agent_tls_over_ble.go` | `NewClientTLSConfig` — the mTLS config for the agent path. `InsecureSkipVerify: true` with real validation in `VerifyConnection`, because there is no hostname over L2CAP and ML-DSA chain certs don't parse in Go's built-in verifier. Depends on `shared/certs`. |
| `lite_client.go` | `LiteClient` — Wendy Lite (ESP32) Wi-Fi provisioning over GATT: write SSID, write password, subscribe to status, write the connect command, wait for `CONNECTED` or `FAILED`. |

## The one that got away

`lite_info.go` — the Wendy Lite GATT info service, carrying the board's L2CAP PSM and the identity it
advertises for itself — belongs with these but lives in
[`internal/shared/ble`](../../shared/ble/lite_info.go). `shared/discovery` reads it to decide whether
a board is worth listing, and a `shared/` package importing a `cli/` one would be an upward edge.
That package is *also* named `ble`; no file currently needs both, and a future one must alias.

## Two independent PSMs, both 128

`ble.DefaultL2CAPPSM` here is the PSM the WendyOS agent listens on. `liteclient.DefaultL2CAPPSM` is
the Wendy Lite firmware's. They are two different programs' listening PSMs — one of them firmware
from another repo — that happen to share a value today, and either is free to move. A PSM is a Wendy
convention rather than a property of BLE, which is why neither lives in `shared/ble/central`.

Prefer the PSM a board publishes over the default: `ble.ReadLiteInfo` reads it over GATT, and the
fallback exists only for platforms or devices that cannot answer.
