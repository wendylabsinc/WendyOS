# iOS Companion SDK Design

**Date:** 2026-03-30
**Status:** Approved

## Overview

A Swift Package SDK for iOS companion apps that communicate with Wendy devices. Supports two transports — BLE (L2CAP) and gRPC over TCP/IP — behind a unified API. Targets iOS 18+, uses async/await throughout.

The SDK lives in a new repository at `../companion-sdk` (i.e. `git/wendy/companion-sdk`), separate from `wendy-agent`.

---

## Package Structure

```
companion-sdk/
├── Package.swift
├── Proto/                        # Copied from wendy-agent repo, kept in sync manually
│   └── wendy/agent/services/v1/
│       ├── shared.proto
│       ├── wendy_agent_v1_service.proto
│       ├── wendy_agent_v1_container_service.proto
│       ├── wendy_agent_v1_audio_service.proto
│       ├── wendy_agent_v1_bluetooth.proto
│       └── wendy_agent_v1_provisioning_service.proto
├── Sources/
│   └── WendyCompanionSDK/
│       ├── Discovery/
│       │   ├── DeviceDiscovery.swift       # CoreBluetooth scanning
│       │   └── DiscoveredDevice.swift      # name, peripheralID, CBPeripheral ref
│       ├── Client/
│       │   ├── WendyDevice.swift           # Top-level connected device handle
│       │   └── WendyConnection.swift       # Transport management (BLE or gRPC)
│       ├── Transport/
│       │   ├── WendyTransport.swift        # Protocol + shared types
│       │   ├── BLETransport.swift          # L2CAP channel, length-prefixed protobuf
│       │   └── GRPCTransport.swift         # grpc-swift channel wrapper
│       └── Services/
│           ├── AppsService.swift
│           ├── WiFiService.swift
│           ├── BluetoothService.swift
│           ├── AudioService.swift
│           └── DeviceInfoService.swift
└── Tests/
    └── WendyCompanionSDKTests/
        ├── AppsServiceTests.swift
        ├── WiFiServiceTests.swift
        └── ...
```

**Dependencies** (same versions as `macos-companion`):
- `grpc-swift` 2.2.3
- `grpc-swift-nio-transport` 1.2.3
- `grpc-swift-protobuf` 1.3.1
- `swift-protobuf` 1.35.1

**Code generation:** Uses the `GRPCProtobufGenerator` build plugin from `grpc-swift-protobuf`. Proto files are committed into the repo; Swift code is generated at build time.

---

## Discovery

`DeviceDiscovery` scans for nearby Wendy devices using CoreBluetooth.

**BLE service UUID:** `7565e9eb-4c20-4b67-9272-d708b397b631`
**L2CAP PSM:** Not yet fixed. Exposed as `WendyBLEConstants.l2capPSM` (placeholder value). `DeviceDiscovery` accepts it as an overrideable parameter.

```swift
let discovery = DeviceDiscovery()
// or: DeviceDiscovery(l2capPSM: 0x1234)

for await peripheral in discovery.peripherals {
    // peripheral: DiscoveredDevice(name, peripheralID, cbPeripheral)
    let device = try await WendyDevice.connect(peripheral: peripheral)
}

discovery.stop()
```

Discovery only surfaces BLE peripherals advertising the Wendy service UUID. IP-based device access does not require discovery — the caller provides host and port directly.

---

## Transports

### BLE Transport (L2CAP)

- Opens a CoreBluetooth L2CAP channel using the published PSM
- **Framing:** `UInt16` big-endian (2 bytes) length prefix, followed by protobuf payload. Length encodes the number of protobuf bytes only (does not include the 2-byte prefix itself)
- **Messages:** `BluetoothCommand` (iOS → device) and `BluetoothResponse` (device → iOS), defined in `wendy_agent_v1_bluetooth.proto`
- Request/response: one command → one response (no server streaming over BLE)

### gRPC Transport (TCP/IP)

- Standard gRPC over TCP using grpc-swift 2.x
- Caller provides `host: String` and `port: Int`
- Full service coverage including streaming RPCs

### WendyDevice

The unified entry point. Internal transport is an implementation detail.

```swift
// BLE
let device = try await WendyDevice.connect(peripheral: discoveredDevice)

// IP
let device = try await WendyDevice.connect(host: "192.168.1.100", port: 50051)

// Services (same API regardless of transport)
device.apps
device.wifi
device.bluetooth
device.info
device.audio    // throws WendyError.notAvailableOnTransport over BLE
```

---

## Service APIs

### Transport availability

| Service / Operation | BLE | gRPC |
|---|---|---|
| `apps.list()` | ✓ | ✓ |
| `apps.stop(appName:)` | ✓ | ✓ |
| `apps.remove(appName:purgeImage:)` | ✓ | ✓ |
| `apps.startApp(named:onOutput:)` | — | ✓ |
| `apps.attachApp(named:onOutput:)` | — | ✓ |
| `wifi.*` | ✓ | ✓ |
| `bluetooth.*` | ✓ | ✓ |
| `info.*` | ✓ | ✓ |
| `audio.*` | — | ✓ |

### AppsService

```swift
func list() async throws -> [App]
func stop(appName: String) async throws
func remove(appName: String, purgeImage: Bool) async throws
func startApp(named appName: String, onOutput: (ConsoleOutput) async throws -> Void) async throws  // IP only
func attachApp(named appName: String, onOutput: (ConsoleOutput) async throws -> Void) async throws // IP only
```

`startApp` and `attachApp` run for the duration of the stream, returning when the stream ends or the closure throws. Cancellable via Swift task cancellation.

### WiFiService

```swift
func listNetworks() async throws -> [WiFiNetwork]
func connect(ssid: String, password: String) async throws
func disconnect() async throws
func status() async throws -> WiFiStatus
```

### BluetoothService

Manages Bluetooth peripherals *on the Wendy device* (not the iOS device).

```swift
func list(pairedOnly: Bool) async throws -> [BluetoothDevice]
func connect(address: String) async throws
func disconnect(address: String) async throws
func forget(address: String) async throws
```

### DeviceInfoService

```swift
func agentVersion() async throws -> AgentVersion
func hardwareCapabilities() async throws -> [HardwareCapability]
```

### AudioService (IP only)

```swift
func listDevices() async throws -> [AudioDevice]
func setDefaultDevice(id: UInt32) async throws
func streamLevels(deviceID: UInt32, onLevel: (AudioLevel) async throws -> Void) async throws
func streamAudio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32, onChunk: (AudioChunk) async throws -> Void) async throws
```

All `AudioService` methods throw `WendyError.notAvailableOnTransport` when called on a BLE-connected device.

---

## Model Types

SDK-native value types, mapped from both transport responses (protobuf types are not exposed in the public API):

- `App` — `appName`, `appVersion`, `runningState: AppRunningState`, `failureCount`
- `WiFiNetwork` — `ssid`, `signalStrength: Int?`
- `WiFiStatus` — `connected: Bool`, `ssid: String?`
- `BluetoothDevice` — `name`, `address`, `rssi: Int?`, `paired`, `connected`, `trusted`, `deviceType`
- `AgentVersion` — `version`, `osVersion: String?`, `os`, `cpuArchitecture`, `featureset: [String]`
- `HardwareCapability` — `category`, `devicePath`, `description`, `properties: [String: String]`
- `AudioDevice` — `id`, `name`, `description`, `type: AudioDeviceType`, `isDefault`
- `AudioLevel` — `peakDB`, `rmsDB`, `timestampNS`
- `AudioChunk` — `pcmData`, `timestampNS`, `sampleRate`, `channels`
- `ConsoleOutput` — `data: Data`, `stream: ConsoleStream` (stdout/stderr)

---

## Error Handling

Single `WendyError` enum thrown across all services:

```swift
enum WendyError: Error {
    case notAvailableOnTransport        // operation not supported on current transport
    case connectionFailed(String)       // failed to connect (gRPC or L2CAP)
    case disconnected                   // connection dropped mid-operation
    case deviceError(String)            // ErrorResponse from BLE; gRPC status error
    case protocolError(String)          // malformed framing or unexpected message
}
```

---

## Testing

Each service takes a `WendyTransport` protocol rather than a concrete type, enabling mock injection in unit tests:

```swift
protocol WendyTransport {
    func send(_ command: BluetoothCommand) async throws -> BluetoothResponse
}
```

The gRPC transport is testable via grpc-swift's own test channel utilities.

Unit tests cover service logic with mock transports. Integration tests (requiring a real device) live in a separate `WendyCompanionSDKIntegrationTests` target and are not run by default.
