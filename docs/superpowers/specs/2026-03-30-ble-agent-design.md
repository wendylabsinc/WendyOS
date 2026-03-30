# BLE Agent Advertising & Communication — Design Spec

**Date:** 2026-03-30
**Status:** Approved

---

## Overview

WendyOS agents currently advertise and accept connections over LAN (mDNS) and USB. This spec adds BLE as a third transport: the agent advertises itself as a BLE peripheral on Linux (via BlueZ D-Bus), accepts L2CAP connections on PSM 128, and dispatches the existing protobuf command protocol. The CLI gains a working Linux BLE client to complement the existing macOS (CoreBluetooth) implementation.

---

## Architecture

The work splits into two independent subsystems sharing only the existing proto protocol (`BluetoothCommand` / `BluetoothResponse`, length-prefixed protobuf over L2CAP PSM 128).

### Agent side (Linux peripheral)

New files in `go/internal/agent/bluetooth/`:

| File | Purpose |
|---|---|
| `advertiser_linux.go` | BlueZ D-Bus `LEAdvertisement1` object + `RegisterAdvertisement` |
| `l2cap_server_linux.go` | `AF_BLUETOOTH SOCK_SEQPACKET` listener and accept loop |
| `l2cap_server_stub.go` | No-op for non-Linux builds |
| `dispatcher.go` | Deserialize `BluetoothCommand` → call service logic → serialize `BluetoothResponse` |

`main.go` gains a single call:

```go
bluetooth.StartBLEPeripheral(ctx, logger, dispatcher)
```

This starts advertising and the L2CAP accept loop as a background goroutine, stopping cleanly on context cancellation.

### CLI side (Linux client)

`go/internal/cli/ble/ble_linux.go` — the current stub is replaced with a real `AF_BLUETOOTH / SOCK_SEQPACKET / BTPROTO_L2CAP` outbound implementation. No changes to `agent_client.go`, `lite_client.go`, or any command files — they use the same `Connection` interface already implemented for Darwin.

---

## Component Details

### Agent: Advertising (`advertiser_linux.go`)

- Registers a D-Bus object at `/org/wendy/advertisement0` implementing `org.bluez.LEAdvertisement1`
- Properties:
  - `Type` = `"peripheral"`
  - `ServiceUUIDs` = `["7565e9eb-4c20-4b67-9272-d708b397b631"]`
  - `LocalName` = OS hostname
  - `Discoverable` = `true`
- Calls `org.bluez.LEAdvertisingManager1.RegisterAdvertisement` on `/org/bluez/hci0`
- On context cancellation: calls `UnregisterAdvertisement`, removes D-Bus object
- Uses `godbus/dbus` (already a dependency via `dbusproxy`)
- No GATT application needed — WendyOS agent communication is entirely over L2CAP, not GATT characteristics

**Failure policy:** If BlueZ is unavailable (no adapter, D-Bus not running, permission denied), log a warning and return. The agent continues serving LAN/USB. BLE advertising is best-effort.

### Agent: L2CAP Server (`l2cap_server_linux.go`)

1. Create `AF_BLUETOOTH / SOCK_SEQPACKET / BTPROTO_L2CAP` socket
2. Set `SO_REUSEADDR`, bind to `{BDADDR_ANY, PSM 128}`
3. `listen()` and enter accept loop
4. Per accepted connection: spawn goroutine, read one length-prefixed protobuf request, dispatch, write response, loop until error or disconnect
5. On context cancellation: close listener, drain in-flight connections

**Failure policy:** If bind fails (PSM in use, permissions), log error and return from `StartBLEPeripheral`. Best-effort, same as advertising.

### Agent: Dispatcher (`dispatcher.go`)

Platform-independent. Switches on the `BluetoothCommand.oneof`:

| Command | Handler |
|---|---|
| `wifi_list` | `NetworkManager.ListWiFiNetworks` |
| `wifi_connect` | `NetworkManager.ConnectToWiFi` |
| `wifi_status` | `NetworkManager.GetWiFiStatus` |
| `wifi_disconnect` | `NetworkManager.DisconnectWiFi` |
| `apps_list` | `ContainerdClient.ListContainers` |
| `apps_stop` | `ContainerdClient.StopContainer` |
| `apps_remove` | `ContainerdClient.DeleteContainer` |
| `agent_version` | inline (reads version + OS info) |
| `hardware_list` | `HardwareDiscoverer.Discover` |
| `bluetooth_list` | `BluetoothManager.Scan` |
| `bluetooth_connect` | `BluetoothManager.Connect` |
| `bluetooth_disconnect` | `BluetoothManager.Disconnect` |
| `bluetooth_forget` | `BluetoothManager.Forget` |

No new service logic — purely wires the BLE path to the same handlers used by gRPC.

### CLI: Linux BLE Client (`ble_linux.go`)

Replaces the current stub. The `Connection` struct holds a single `int` file descriptor.

**`Connect(address string, timeoutSeconds int) (*Connection, error)`**
- Parses `"AA:BB:CC:DD:EE:FF"` into `[6]byte`
- Creates `AF_BLUETOOTH / SOCK_SEQPACKET / BTPROTO_L2CAP` socket
- Stores fd for use by `OpenL2CAP`

**`OpenL2CAP(psm uint16, timeoutSeconds int) error`**
- `connect()` syscall with remote address + PSM and a `SO_SNDTIMEO` deadline
- On Linux, connect and L2CAP channel open are a single operation

**`L2CAPSend(data []byte) error`** — `write()` syscall

**`L2CAPRecv(timeoutSeconds int) ([]byte, error)`** — `read()` syscall with `SO_RCVTIMEO`

**`Close()`** — `close()` syscall

**GATT methods** (`DiscoverServices`, `WriteCharacteristic`, etc.) — return `"not implemented"`. Wendy Lite GATT support on Linux CLI is out of scope.

---

## Error Handling

| Scenario | Behaviour |
|---|---|
| BlueZ unavailable / no adapter | Log warning, skip BLE, continue |
| L2CAP bind failure | Log error, skip BLE, continue |
| Malformed protobuf from client | Send `ErrorResponse`, close connection |
| Unknown command type | Send `ErrorResponse`, close connection |
| Service error (e.g. WiFi failed) | Send `ErrorResponse` with message, close connection |
| Context cancellation | Unregister advertisement, close listener, wait for in-flight connections |

---

## Testing

**Unit tests (`dispatcher_test.go`):** Construct a `BluetoothCommand`, call the dispatcher with mock `NetworkManager` / `BluetoothManager` / `HardwareDiscoverer`, assert the `BluetoothResponse`. Covers all 13 command paths without BLE hardware.

**Integration tests:** `advertiser_linux.go` and `l2cap_server_linux.go` require BlueZ/hardware — no unit tests for now. Build-tag isolation (`//go:build linux`) keeps them out of CI on other platforms.

**CLI client:** `ble_linux.go` is integration-only. Existing `agent_client.go` logic is unchanged and covered by existing tests.

---

## Files Changed

| File | Change |
|---|---|
| `go/internal/agent/bluetooth/advertiser_linux.go` | New |
| `go/internal/agent/bluetooth/l2cap_server_linux.go` | New |
| `go/internal/agent/bluetooth/l2cap_server_stub.go` | New |
| `go/internal/agent/bluetooth/dispatcher.go` | New |
| `go/internal/agent/bluetooth/dispatcher_test.go` | New |
| `go/internal/agent/bluetooth/manager.go` | Add `StartBLEPeripheral` function |
| `go/internal/agent/services/interfaces.go` | Expose service interfaces needed by dispatcher |
| `go/internal/cli/ble/ble_linux.go` | Replace stub with real implementation |
| `go/cmd/wendy-agent/main.go` | Call `StartBLEPeripheral` |

---

## Out of Scope

- Wendy Lite (ESP32) GATT client on Linux CLI
- BLE advertising on macOS agent (macOS does not run WendyOS)
- Windows BLE client
- mTLS over BLE
