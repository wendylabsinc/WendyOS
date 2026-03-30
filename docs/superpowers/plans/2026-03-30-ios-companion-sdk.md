# iOS Companion SDK Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Swift Package SDK at `../companion-sdk` (i.e. `/Users/joannisorlandos/git/wendy/companion-sdk`) for iOS 18 companion apps, exposing Wendy device management over both BLE (L2CAP) and gRPC/TCP behind a unified async/await API.

**Architecture:** A single `WendyCompanionSDK` public library backed by two internal targets: `WendyProtos` (build-plugin-generated protobuf/gRPC Swift code) and `WendyCompanionSDK` (handwritten Swift). Services hold a per-service transport protocol, enabling mock injection for unit tests. BLE uses CoreBluetooth L2CAP with `UInt16` big-endian framing; gRPC uses grpc-swift 2.x HTTP/2 over TCP (Network.framework on iOS).

**Tech Stack:** Swift 6, iOS 18+, swift-tools-version 6.0, grpc-swift 2.2.3, grpc-swift-nio-transport 1.2.3, grpc-swift-protobuf 1.3.1, swift-protobuf 1.35.1, CoreBluetooth, Swift Testing

---

## File Map

```
companion-sdk/
├── .gitignore
├── Package.swift
├── Sources/
│   ├── WendyProtos/                         # Internal target: generated proto code
│   │   ├── grpc-swift-protobuf.json         # Build plugin config
│   │   └── wendy/agent/services/v1/         # Proto files (proto import paths preserved)
│   │       ├── shared.proto
│   │       ├── wendy_agent_v1_bluetooth.proto
│   │       ├── wendy_agent_v1_service.proto
│   │       ├── wendy_agent_v1_container_service.proto
│   │       └── wendy_agent_v1_audio_service.proto
│   └── WendyCompanionSDK/
│       ├── WendyError.swift                 # WendyError enum
│       ├── Constants.swift                  # WendyBLEConstants (UUID, PSM, default port)
│       ├── Models/
│       │   ├── AppModels.swift              # App, AppRunningState, ConsoleOutput, ConsoleStream
│       │   ├── WiFiModels.swift             # WiFiNetwork, WiFiStatus
│       │   ├── BluetoothModels.swift        # BluetoothDevice
│       │   ├── DeviceInfoModels.swift       # AgentVersion, HardwareCapability
│       │   └── AudioModels.swift           # AudioDevice, AudioDeviceType, AudioLevel, AudioChunk
│       ├── Transport/
│       │   ├── TransportProtocols.swift     # AppsTransporting, WiFiTransporting, etc.
│       │   ├── BLEFraming.swift             # Stateless encode/decode functions
│       │   ├── BLEChannel.swift             # Actor: L2CAP stream + StreamDelegate bridge
│       │   ├── BLETransport.swift           # Actor: conforms to BLE-supported protocols
│       │   └── GRPCTransport.swift          # Actor: conforms to all protocols via gRPC
│       ├── Discovery/
│       │   ├── DiscoveredDevice.swift       # DiscoveredDevice struct
│       │   └── DeviceDiscovery.swift        # @MainActor CBCentralManager wrapper
│       ├── Services/
│       │   ├── AppsService.swift
│       │   ├── WiFiService.swift
│       │   ├── BluetoothService.swift
│       │   ├── DeviceInfoService.swift
│       │   └── AudioService.swift
│       └── WendyDevice.swift                # Public entry point
└── Tests/
    └── WendyCompanionSDKTests/
        ├── Mocks/
        │   └── MockTransports.swift         # Mock conformances for all protocols
        ├── BLEFramingTests.swift
        ├── AppsServiceTests.swift
        ├── WiFiServiceTests.swift
        ├── BluetoothServiceTests.swift
        ├── DeviceInfoServiceTests.swift
        └── AudioServiceTests.swift
```

---

## Task 1: Initialize repo and Package.swift

**Files:**
- Create: `companion-sdk/.gitignore`
- Create: `companion-sdk/Package.swift`

- [ ] **Step 1: Create repo**

```bash
cd /Users/joannisorlandos/git/wendy
mkdir companion-sdk
cd companion-sdk
git init
```

- [ ] **Step 2: Write .gitignore**

```bash
cat > .gitignore << 'EOF'
.DS_Store
/.build
/Packages
xcuserdata/
*.xcodeproj
*.xcworkspace
EOF
```

- [ ] **Step 3: Write Package.swift**

```swift
// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "WendyCompanionSDK",
    platforms: [
        .iOS(.v18)
    ],
    products: [
        .library(name: "WendyCompanionSDK", targets: ["WendyCompanionSDK"])
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift.git", from: "2.2.3"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "1.2.3"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "1.3.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.35.1"),
    ],
    targets: [
        // Internal target: build plugin generates Swift from .proto files here
        .target(
            name: "WendyProtos",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            plugins: [
                .plugin(name: "GRPCProtobufGenerator", package: "grpc-swift-protobuf")
            ]
        ),
        // Public SDK library
        .target(
            name: "WendyCompanionSDK",
            dependencies: [
                "WendyProtos",
                .product(name: "GRPCCore", package: "grpc-swift"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
            ]
        ),
        .testTarget(
            name: "WendyCompanionSDKTests",
            dependencies: ["WendyCompanionSDK"]
        )
    ]
)
```

- [ ] **Step 4: Create directory structure**

```bash
mkdir -p Sources/WendyProtos/wendy/agent/services/v1
mkdir -p Sources/WendyCompanionSDK/Models
mkdir -p Sources/WendyCompanionSDK/Transport
mkdir -p Sources/WendyCompanionSDK/Discovery
mkdir -p Sources/WendyCompanionSDK/Services
mkdir -p Tests/WendyCompanionSDKTests/Mocks
```

- [ ] **Step 5: Initial commit**

```bash
git add Package.swift .gitignore
git commit -m "chore: initialize WendyCompanionSDK Swift package"
```

---

## Task 2: Copy proto files and configure build plugin

**Files:**
- Create: `Sources/WendyProtos/grpc-swift-protobuf.json`
- Create: `Sources/WendyProtos/wendy/agent/services/v1/shared.proto` (copied)
- Create: `Sources/WendyProtos/wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto` (copied)
- Create: `Sources/WendyProtos/wendy/agent/services/v1/wendy_agent_v1_service.proto` (copied)
- Create: `Sources/WendyProtos/wendy/agent/services/v1/wendy_agent_v1_container_service.proto` (copied)
- Create: `Sources/WendyProtos/wendy/agent/services/v1/wendy_agent_v1_audio_service.proto` (copied)

- [ ] **Step 1: Copy proto files from wendy-agent**

```bash
PROTOS_SRC=/Users/joannisorlandos/git/wendy/wendy-agent/Proto/wendy/agent/services/v1
PROTOS_DST=Sources/WendyProtos/wendy/agent/services/v1

cp $PROTOS_SRC/shared.proto $PROTOS_DST/
cp $PROTOS_SRC/wendy_agent_v1_bluetooth.proto $PROTOS_DST/
cp $PROTOS_SRC/wendy_agent_v1_service.proto $PROTOS_DST/
cp $PROTOS_SRC/wendy_agent_v1_container_service.proto $PROTOS_DST/
cp $PROTOS_SRC/wendy_agent_v1_audio_service.proto $PROTOS_DST/
```

- [ ] **Step 2: Write grpc-swift-protobuf.json**

The plugin looks for this file in the target source root (`Sources/WendyProtos/`). Proto file paths are relative to that root, preserving the import paths used within the proto files.

```json
{
  "invocations": [
    {
      "protoFiles": [
        "wendy/agent/services/v1/shared.proto",
        "wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto",
        "wendy/agent/services/v1/wendy_agent_v1_service.proto",
        "wendy/agent/services/v1/wendy_agent_v1_container_service.proto",
        "wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
      ],
      "visibility": "internal",
      "client": true,
      "server": false
    }
  ]
}
```

- [ ] **Step 3: Verify build succeeds**

```bash
swift build
```

Expected: Build succeeds. If it fails with plugin config errors, check the grpc-swift-protobuf README for the exact JSON format for version 1.3.1.

- [ ] **Step 4: Inspect generated type names**

```bash
find .build -name "*.pb.swift" -o -name "*.grpc.swift" | head -20
# Open one of the generated files to confirm type names:
# - AppContainer, RestartPolicy, AppRunningState (from shared.proto)
# - BluetoothCommand, BluetoothResponse (from bluetooth.proto)
# - WendyContainerService_Client or similar (from container_service.proto)
# Note the exact type names — they will be used in BLETransport and GRPCTransport.
```

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyProtos/
git commit -m "feat: add proto files and build plugin config"
```

---

## Task 3: WendyError and Constants

**Files:**
- Create: `Sources/WendyCompanionSDK/WendyError.swift`
- Create: `Sources/WendyCompanionSDK/Constants.swift`

- [ ] **Step 1: Write WendyError.swift**

```swift
// Sources/WendyCompanionSDK/WendyError.swift
public enum WendyError: Error, Sendable {
    /// The requested operation is not supported over the current transport (e.g. audio over BLE).
    case notAvailableOnTransport
    /// Failed to establish a connection (gRPC or L2CAP).
    case connectionFailed(String)
    /// The connection dropped mid-operation.
    case disconnected
    /// The device returned an error response.
    case deviceError(String)
    /// Malformed framing or an unexpected message type was received.
    case protocolError(String)
}
```

- [ ] **Step 2: Write Constants.swift**

```swift
// Sources/WendyCompanionSDK/Constants.swift
import CoreBluetooth

public enum WendyBLEConstants {
    /// The BLE service UUID advertised by Wendy devices.
    public static let serviceUUID = CBUUID(string: "7565e9eb-4c20-4b67-9272-d708b397b631")

    /// The L2CAP PSM published by the Wendy device.
    /// This value is not yet finalized — override via DeviceDiscovery(l2capPSM:).
    public static let l2capPSM: CBL2CAPPSM = 0x0080
}

public enum WendyDefaults {
    /// Default gRPC port for Wendy agent over TCP.
    public static let grpcPort = 50051
}
```

- [ ] **Step 3: Verify build**

```bash
swift build
```

Expected: Build succeeds.

- [ ] **Step 4: Commit**

```bash
git add Sources/WendyCompanionSDK/WendyError.swift Sources/WendyCompanionSDK/Constants.swift
git commit -m "feat: add WendyError and BLE/gRPC constants"
```

---

## Task 4: Model types

**Files:**
- Create: `Sources/WendyCompanionSDK/Models/AppModels.swift`
- Create: `Sources/WendyCompanionSDK/Models/WiFiModels.swift`
- Create: `Sources/WendyCompanionSDK/Models/BluetoothModels.swift`
- Create: `Sources/WendyCompanionSDK/Models/DeviceInfoModels.swift`
- Create: `Sources/WendyCompanionSDK/Models/AudioModels.swift`

- [ ] **Step 1: Write AppModels.swift**

```swift
// Sources/WendyCompanionSDK/Models/AppModels.swift
public enum AppRunningState: Sendable {
    case stopped
    case running
}

public struct App: Sendable {
    public let appName: String
    public let appVersion: String
    public let runningState: AppRunningState
    public let failureCount: UInt32

    public init(appName: String, appVersion: String, runningState: AppRunningState, failureCount: UInt32) {
        self.appName = appName
        self.appVersion = appVersion
        self.runningState = runningState
        self.failureCount = failureCount
    }
}

public enum ConsoleStream: Sendable {
    case stdout
    case stderr
}

public struct ConsoleOutput: Sendable {
    public let data: Data
    public let stream: ConsoleStream

    public init(data: Data, stream: ConsoleStream) {
        self.data = data
        self.stream = stream
    }
}
```

- [ ] **Step 2: Write WiFiModels.swift**

```swift
// Sources/WendyCompanionSDK/Models/WiFiModels.swift
public struct WiFiNetwork: Sendable {
    public let ssid: String
    public let signalStrength: Int?

    public init(ssid: String, signalStrength: Int?) {
        self.ssid = ssid
        self.signalStrength = signalStrength
    }
}

public struct WiFiStatus: Sendable {
    public let connected: Bool
    public let ssid: String?

    public init(connected: Bool, ssid: String?) {
        self.connected = connected
        self.ssid = ssid
    }
}
```

- [ ] **Step 3: Write BluetoothModels.swift**

```swift
// Sources/WendyCompanionSDK/Models/BluetoothModels.swift
public struct BluetoothDevice: Sendable {
    public let name: String
    public let address: String
    public let rssi: Int?
    public let paired: Bool
    public let connected: Bool
    public let trusted: Bool
    public let deviceType: String

    public init(name: String, address: String, rssi: Int?, paired: Bool, connected: Bool, trusted: Bool, deviceType: String) {
        self.name = name
        self.address = address
        self.rssi = rssi
        self.paired = paired
        self.connected = connected
        self.trusted = trusted
        self.deviceType = deviceType
    }
}
```

- [ ] **Step 4: Write DeviceInfoModels.swift**

```swift
// Sources/WendyCompanionSDK/Models/DeviceInfoModels.swift
public struct AgentVersion: Sendable {
    public let version: String
    public let osVersion: String?
    public let os: String
    public let cpuArchitecture: String
    public let featureset: [String]

    public init(version: String, osVersion: String?, os: String, cpuArchitecture: String, featureset: [String]) {
        self.version = version
        self.osVersion = osVersion
        self.os = os
        self.cpuArchitecture = cpuArchitecture
        self.featureset = featureset
    }
}

public struct HardwareCapability: Sendable {
    public let category: String
    public let devicePath: String
    public let details: String
    public let properties: [String: String]

    public init(category: String, devicePath: String, details: String, properties: [String: String]) {
        self.category = category
        self.devicePath = devicePath
        self.details = details
        self.properties = properties
    }
}
```

- [ ] **Step 5: Write AudioModels.swift**

```swift
// Sources/WendyCompanionSDK/Models/AudioModels.swift
public enum AudioDeviceType: Sendable {
    case input
    case output
}

public struct AudioDevice: Sendable {
    public let id: UInt32
    public let name: String
    public let details: String
    public let type: AudioDeviceType
    public let isDefault: Bool

    public init(id: UInt32, name: String, details: String, type: AudioDeviceType, isDefault: Bool) {
        self.id = id
        self.name = name
        self.details = details
        self.type = type
        self.isDefault = isDefault
    }
}

public struct AudioLevel: Sendable {
    public let peakDB: Float
    public let rmsDB: Float
    public let timestampNS: UInt64

    public init(peakDB: Float, rmsDB: Float, timestampNS: UInt64) {
        self.peakDB = peakDB
        self.rmsDB = rmsDB
        self.timestampNS = timestampNS
    }
}

public struct AudioChunk: Sendable {
    public let pcmData: Data
    public let timestampNS: UInt64
    public let sampleRate: UInt32
    public let channels: UInt32

    public init(pcmData: Data, timestampNS: UInt64, sampleRate: UInt32, channels: UInt32) {
        self.pcmData = pcmData
        self.timestampNS = timestampNS
        self.sampleRate = sampleRate
        self.channels = channels
    }
}
```

- [ ] **Step 6: Verify build**

```bash
swift build
```

Expected: Build succeeds.

- [ ] **Step 7: Commit**

```bash
git add Sources/WendyCompanionSDK/Models/
git commit -m "feat: add public model types"
```

---

## Task 5: Transport protocols and mock implementations

**Files:**
- Create: `Sources/WendyCompanionSDK/Transport/TransportProtocols.swift`
- Create: `Tests/WendyCompanionSDKTests/Mocks/MockTransports.swift`

- [ ] **Step 1: Write TransportProtocols.swift**

```swift
// Sources/WendyCompanionSDK/Transport/TransportProtocols.swift
protocol AppsTransporting: Sendable {
    func listApps() async throws -> [App]
    func stopApp(named name: String) async throws
    func removeApp(named name: String, purgeImage: Bool) async throws
    func startApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws
    func attachApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws
}

protocol WiFiTransporting: Sendable {
    func listNetworks() async throws -> [WiFiNetwork]
    func connect(ssid: String, password: String) async throws
    func disconnect() async throws
    func status() async throws -> WiFiStatus
}

protocol BluetoothTransporting: Sendable {
    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDevice]
    func connect(address: String) async throws
    func disconnect(address: String) async throws
    func forget(address: String) async throws
}

protocol DeviceInfoTransporting: Sendable {
    func agentVersion() async throws -> AgentVersion
    func hardwareCapabilities() async throws -> [HardwareCapability]
}

protocol AudioTransporting: Sendable {
    func listDevices() async throws -> [AudioDevice]
    func setDefaultDevice(id: UInt32) async throws
    func streamLevels(deviceID: UInt32, onLevel: (AudioLevel) async throws -> Void) async throws
    func streamAudio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32, onChunk: (AudioChunk) async throws -> Void) async throws
}
```

- [ ] **Step 2: Write failing test**

```swift
// Tests/WendyCompanionSDKTests/Mocks/MockTransports.swift
import Testing
@testable import WendyCompanionSDK

struct MockAppsTransport: AppsTransporting {
    var stubbedApps: [App] = []
    var stopError: Error? = nil
    var removeError: Error? = nil

    func listApps() async throws -> [App] { stubbedApps }
    func stopApp(named name: String) async throws {
        if let error = stopError { throw error }
    }
    func removeApp(named name: String, purgeImage: Bool) async throws {
        if let error = removeError { throw error }
    }
    func startApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        throw WendyError.notAvailableOnTransport
    }
    func attachApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        throw WendyError.notAvailableOnTransport
    }
}

struct MockWiFiTransport: WiFiTransporting {
    var stubbedNetworks: [WiFiNetwork] = []
    var stubbedStatus = WiFiStatus(connected: false, ssid: nil)

    func listNetworks() async throws -> [WiFiNetwork] { stubbedNetworks }
    func connect(ssid: String, password: String) async throws {}
    func disconnect() async throws {}
    func status() async throws -> WiFiStatus { stubbedStatus }
}

struct MockBluetoothTransport: BluetoothTransporting {
    var stubbedDevices: [BluetoothDevice] = []

    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDevice] {
        pairedOnly ? stubbedDevices.filter { $0.paired } : stubbedDevices
    }
    func connect(address: String) async throws {}
    func disconnect(address: String) async throws {}
    func forget(address: String) async throws {}
}

struct MockDeviceInfoTransport: DeviceInfoTransporting {
    var stubbedVersion = AgentVersion(version: "1.0", osVersion: nil, os: "linux", cpuArchitecture: "arm64", featureset: [])
    var stubbedCapabilities: [HardwareCapability] = []

    func agentVersion() async throws -> AgentVersion { stubbedVersion }
    func hardwareCapabilities() async throws -> [HardwareCapability] { stubbedCapabilities }
}

struct MockAudioTransport: AudioTransporting {
    var stubbedDevices: [AudioDevice] = []

    func listDevices() async throws -> [AudioDevice] { stubbedDevices }
    func setDefaultDevice(id: UInt32) async throws {}
    func streamLevels(deviceID: UInt32, onLevel: (AudioLevel) async throws -> Void) async throws {}
    func streamAudio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32, onChunk: (AudioChunk) async throws -> Void) async throws {}
}
```

- [ ] **Step 3: Verify build and test**

```bash
swift build && swift test
```

Expected: Build succeeds. No tests yet (file has no `@Test` functions), but it verifies mock conformances compile.

- [ ] **Step 4: Commit**

```bash
git add Sources/WendyCompanionSDK/Transport/TransportProtocols.swift
git add Tests/WendyCompanionSDKTests/Mocks/MockTransports.swift
git commit -m "feat: add transport protocols and mock implementations"
```

---

## Task 6: BLE message framing

**Files:**
- Create: `Sources/WendyCompanionSDK/Transport/BLEFraming.swift`
- Create: `Tests/WendyCompanionSDKTests/BLEFramingTests.swift`

- [ ] **Step 1: Write failing test**

```swift
// Tests/WendyCompanionSDKTests/BLEFramingTests.swift
import Testing
@testable import WendyCompanionSDK
import WendyProtos

@Suite struct BLEFramingTests {
    @Test func encodeProducesLengthPrefixedPayload() throws {
        var command = BluetoothCommand()
        command.command = .appsList(AppsListCommand())
        let framed = try BLEFraming.encode(command)

        let payload = try command.serializedData()
        #expect(framed.count == 2 + payload.count)
        let length = Int(framed[0]) << 8 | Int(framed[1])
        #expect(length == payload.count)
        #expect(framed[2...] == payload[...])
    }

    @Test func decodeReturnsNilWhenBufferTooShort() throws {
        var buffer = Data([0x00])  // only 1 byte, need at least 2 for length
        let result = try BLEFraming.decode(from: &buffer)
        #expect(result == nil)
        #expect(buffer.count == 1)  // buffer unchanged
    }

    @Test func decodeReturnsNilWhenPayloadIncomplete() throws {
        var buffer = Data([0x00, 0x05, 0x01, 0x02])  // length=5 but only 2 payload bytes
        let result = try BLEFraming.decode(from: &buffer)
        #expect(result == nil)
        #expect(buffer.count == 4)  // buffer unchanged
    }

    @Test func encodeDecodeRoundTrip() throws {
        var command = BluetoothCommand()
        var stopCmd = AppsStopCommand()
        stopCmd.appName = "my-app"
        command.command = .appsStop(stopCmd)

        let framed = try BLEFraming.encode(command)
        var buffer = framed
        let decoded = try BLEFraming.decode(from: &buffer)

        #expect(decoded != nil)
        #expect(buffer.isEmpty)  // all bytes consumed
        if case .appsStop(let stop) = decoded?.command {
            #expect(stop.appName == "my-app")
        } else {
            Issue.record("Expected appsStop command")
        }
    }

    @Test func decodeConsumesExactlyOneMessageFromBuffer() throws {
        var cmd1 = BluetoothCommand()
        cmd1.command = .appsList(AppsListCommand())
        var cmd2 = BluetoothCommand()
        cmd2.command = .wifiStatus(WifiStatusCommand())

        let framed1 = try BLEFraming.encode(cmd1)
        let framed2 = try BLEFraming.encode(cmd2)
        var buffer = framed1 + framed2

        let first = try BLEFraming.decode(from: &buffer)
        #expect(first != nil)
        #expect(buffer.count == framed2.count)

        let second = try BLEFraming.decode(from: &buffer)
        #expect(second != nil)
        #expect(buffer.isEmpty)
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter BLEFramingTests
```

Expected: FAIL — `BLEFraming` does not exist yet.

- [ ] **Step 3: Write BLEFraming.swift**

```swift
// Sources/WendyCompanionSDK/Transport/BLEFraming.swift
import WendyProtos
import SwiftProtobuf

enum BLEFraming {
    /// Serializes a BluetoothCommand to a length-prefixed frame.
    /// Frame layout: [length_hi, length_lo, ...protobuf_payload]
    /// The length field is UInt16 big-endian and encodes only the payload bytes.
    static func encode(_ command: BluetoothCommand) throws -> Data {
        let payload = try command.serializedData()
        guard payload.count <= Int(UInt16.max) else {
            throw WendyError.protocolError("Command payload too large: \(payload.count) bytes")
        }
        var frame = Data(capacity: 2 + payload.count)
        frame.append(UInt8(payload.count >> 8))
        frame.append(UInt8(payload.count & 0xFF))
        frame.append(contentsOf: payload)
        return frame
    }

    /// Attempts to parse one BluetoothResponse from the front of the buffer.
    /// Returns nil if the buffer does not yet contain a complete message.
    /// On success, the consumed bytes are removed from `buffer`.
    static func decode(from buffer: inout Data) throws -> BluetoothResponse? {
        guard buffer.count >= 2 else { return nil }
        let length = Int(buffer[0]) << 8 | Int(buffer[1])
        guard buffer.count >= 2 + length else { return nil }
        let payload = buffer[2..<(2 + length)]
        let response = try BluetoothResponse(serializedBytes: payload)
        buffer.removeFirst(2 + length)
        return response
    }
}
```

- [ ] **Step 4: Run test to confirm pass**

```bash
swift test --filter BLEFramingTests
```

Expected: All 5 tests pass.

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyCompanionSDK/Transport/BLEFraming.swift
git add Tests/WendyCompanionSDKTests/BLEFramingTests.swift
git commit -m "feat: add BLE message framing (encode/decode)"
```

---

## Task 7: BLE channel (L2CAP stream management)

**Files:**
- Create: `Sources/WendyCompanionSDK/Transport/BLEChannel.swift`

No unit tests for this file — it is tightly coupled to `CBL2CAPChannel` streams which require hardware. Covered by integration tests.

- [ ] **Step 1: Write BLEChannel.swift**

```swift
// Sources/WendyCompanionSDK/Transport/BLEChannel.swift
import CoreBluetooth
import WendyProtos

/// Manages a CoreBluetooth L2CAP channel.
/// Sends BluetoothCommand frames and returns BluetoothResponse frames.
/// One pending request at a time (BLE is request-response).
actor BLEChannel {
    private let outputStream: OutputStream
    private let inputStream: InputStream
    private var readBuffer = Data()
    private var pendingContinuation: CheckedContinuation<BluetoothResponse, Error>?
    private var streamDelegate: BLEStreamDelegate?

    init(channel: CBL2CAPChannel) {
        self.outputStream = channel.outputStream
        self.inputStream = channel.inputStream
    }

    func open() {
        let delegate = BLEStreamDelegate(channel: self)
        self.streamDelegate = delegate

        inputStream.delegate = delegate
        outputStream.delegate = delegate
        inputStream.schedule(in: .main, forMode: .default)
        outputStream.schedule(in: .main, forMode: .default)
        inputStream.open()
        outputStream.open()
    }

    func close() {
        inputStream.close()
        outputStream.close()
        inputStream.remove(from: .main, forMode: .default)
        outputStream.remove(from: .main, forMode: .default)
        pendingContinuation?.resume(throwing: WendyError.disconnected)
        pendingContinuation = nil
    }

    /// Sends a command and waits for the corresponding response.
    func send(_ command: BluetoothCommand) async throws -> BluetoothResponse {
        let frame = try BLEFraming.encode(command)
        try writeFrame(frame)
        return try await withCheckedThrowingContinuation { continuation in
            pendingContinuation = continuation
        }
    }

    private func writeFrame(_ frame: Data) throws {
        let bytes = [UInt8](frame)
        let written = outputStream.write(bytes, maxLength: bytes.count)
        guard written == bytes.count else {
            throw WendyError.connectionFailed("L2CAP write failed (wrote \(written)/\(bytes.count) bytes)")
        }
    }

    // Called by BLEStreamDelegate on the main thread
    func didReceiveBytes(_ bytes: Data) {
        readBuffer.append(bytes)
        do {
            if let response = try BLEFraming.decode(from: &readBuffer) {
                pendingContinuation?.resume(returning: response)
                pendingContinuation = nil
            }
        } catch {
            pendingContinuation?.resume(throwing: WendyError.protocolError(error.localizedDescription))
            pendingContinuation = nil
        }
    }

    // Called by BLEStreamDelegate on the main thread
    func didEncounterError(_ error: Error) {
        pendingContinuation?.resume(throwing: error)
        pendingContinuation = nil
    }
}

/// Stream delegate that bridges Foundation stream callbacks to BLEChannel (an actor).
/// Registered on the main run loop, so callbacks fire on the main thread.
private final class BLEStreamDelegate: NSObject, StreamDelegate, @unchecked Sendable {
    private let channel: BLEChannel

    init(channel: BLEChannel) {
        self.channel = channel
    }

    func stream(_ aStream: Stream, handle eventCode: Stream.Event) {
        switch eventCode {
        case .hasBytesAvailable:
            guard let inputStream = aStream as? InputStream else { return }
            var buffer = [UInt8](repeating: 0, count: 4096)
            let bytesRead = inputStream.read(&buffer, maxLength: buffer.count)
            guard bytesRead > 0 else { return }
            let data = Data(buffer[0..<bytesRead])
            Task { await channel.didReceiveBytes(data) }

        case .errorOccurred:
            let error = aStream.streamError.map { WendyError.connectionFailed($0.localizedDescription) }
                ?? WendyError.disconnected
            Task { await channel.didEncounterError(error) }

        case .endEncountered:
            Task { await channel.didEncounterError(WendyError.disconnected) }

        default:
            break
        }
    }
}
```

- [ ] **Step 2: Verify build**

```bash
swift build
```

Expected: Build succeeds.

- [ ] **Step 3: Commit**

```bash
git add Sources/WendyCompanionSDK/Transport/BLEChannel.swift
git commit -m "feat: add BLE L2CAP channel with stream management"
```

---

## Task 8: BLE transport conformances

**Files:**
- Create: `Sources/WendyCompanionSDK/Transport/BLETransport.swift`

- [ ] **Step 1: Write BLETransport.swift**

This file maps `BluetoothCommand`/`BluetoothResponse` types to SDK model types and conforms to all BLE-supported transport protocols. Audio and streaming app operations throw `notAvailableOnTransport`.

```swift
// Sources/WendyCompanionSDK/Transport/BLETransport.swift
import WendyProtos

actor BLETransport {
    let channel: BLEChannel

    init(channel: BLEChannel) {
        self.channel = channel
    }

    private func send(_ command: BluetoothCommand) async throws -> BluetoothResponse {
        let response = try await channel.send(command)
        if case .error(let errorResponse) = response.response {
            throw WendyError.deviceError(errorResponse.message)
        }
        return response
    }
}

extension BLETransport: AppsTransporting {
    func listApps() async throws -> [App] {
        var command = BluetoothCommand()
        command.command = .appsList(AppsListCommand())
        let response = try await send(command)
        guard case .appsList(let r) = response.response else {
            throw WendyError.protocolError("Expected appsList response")
        }
        return r.apps.map { App(
            appName: $0.appName,
            appVersion: $0.appVersion,
            runningState: $0.state == "running" ? .running : .stopped,
            failureCount: UInt32($0.failureCount)
        )}
    }

    func stopApp(named name: String) async throws {
        var command = BluetoothCommand()
        var stop = AppsStopCommand()
        stop.appName = name
        command.command = .appsStop(stop)
        let response = try await send(command)
        guard case .appsStop(let r) = response.response else {
            throw WendyError.protocolError("Expected appsStop response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "Stop failed")
        }
    }

    func removeApp(named name: String, purgeImage: Bool) async throws {
        var command = BluetoothCommand()
        var remove = AppsRemoveCommand()
        remove.appName = name
        remove.purgeImage = purgeImage
        command.command = .appsRemove(remove)
        let response = try await send(command)
        guard case .appsRemove(let r) = response.response else {
            throw WendyError.protocolError("Expected appsRemove response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "Remove failed")
        }
    }

    func startApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        throw WendyError.notAvailableOnTransport
    }

    func attachApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        throw WendyError.notAvailableOnTransport
    }
}

extension BLETransport: WiFiTransporting {
    func listNetworks() async throws -> [WiFiNetwork] {
        var command = BluetoothCommand()
        command.command = .wifiList(WifiListCommand())
        let response = try await send(command)
        guard case .wifiList(let r) = response.response else {
            throw WendyError.protocolError("Expected wifiList response")
        }
        return r.networks.map { WiFiNetwork(ssid: $0.ssid, signalStrength: $0.hasSignalStrength ? Int($0.signalStrength) : nil) }
    }

    func connect(ssid: String, password: String) async throws {
        var command = BluetoothCommand()
        var connect = WifiConnectCommand()
        connect.ssid = ssid
        connect.password = password
        command.command = .wifiConnect(connect)
        let response = try await send(command)
        guard case .wifiConnect(let r) = response.response else {
            throw WendyError.protocolError("Expected wifiConnect response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "WiFi connect failed")
        }
    }

    func disconnect() async throws {
        var command = BluetoothCommand()
        command.command = .wifiDisconnect(WifiDisconnectCommand())
        let response = try await send(command)
        guard case .wifiDisconnect(let r) = response.response else {
            throw WendyError.protocolError("Expected wifiDisconnect response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "WiFi disconnect failed")
        }
    }

    func status() async throws -> WiFiStatus {
        var command = BluetoothCommand()
        command.command = .wifiStatus(WifiStatusCommand())
        let response = try await send(command)
        guard case .wifiStatus(let r) = response.response else {
            throw WendyError.protocolError("Expected wifiStatus response")
        }
        return WiFiStatus(connected: r.connected, ssid: r.hasSsid ? r.ssid : nil)
    }
}

extension BLETransport: BluetoothTransporting {
    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDevice] {
        var command = BluetoothCommand()
        var list = BluetoothListCommand()
        list.pairedOnly = pairedOnly
        command.command = .bluetoothList(list)
        let response = try await send(command)
        guard case .bluetoothList(let r) = response.response else {
            throw WendyError.protocolError("Expected bluetoothList response")
        }
        return r.devices.map { BluetoothDevice(
            name: $0.name,
            address: $0.address,
            rssi: $0.hasRssi ? Int($0.rssi) : nil,
            paired: $0.paired,
            connected: $0.connected,
            trusted: $0.trusted,
            deviceType: $0.deviceType
        )}
    }

    func connect(address: String) async throws {
        var command = BluetoothCommand()
        var connect = BluetoothConnectCommand()
        connect.address = address
        command.command = .bluetoothConnect(connect)
        let response = try await send(command)
        guard case .bluetoothConnect(let r) = response.response else {
            throw WendyError.protocolError("Expected bluetoothConnect response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "Bluetooth connect failed")
        }
    }

    func disconnect(address: String) async throws {
        var command = BluetoothCommand()
        var disconnect = BluetoothDisconnectCommand()
        disconnect.address = address
        command.command = .bluetoothDisconnect(disconnect)
        let response = try await send(command)
        guard case .bluetoothDisconnect(let r) = response.response else {
            throw WendyError.protocolError("Expected bluetoothDisconnect response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "Bluetooth disconnect failed")
        }
    }

    func forget(address: String) async throws {
        var command = BluetoothCommand()
        var forget = BluetoothForgetCommand()
        forget.address = address
        command.command = .bluetoothForget(forget)
        let response = try await send(command)
        guard case .bluetoothForget(let r) = response.response else {
            throw WendyError.protocolError("Expected bluetoothForget response")
        }
        if !r.success {
            throw WendyError.deviceError(r.errorMessage ?? "Bluetooth forget failed")
        }
    }
}

extension BLETransport: DeviceInfoTransporting {
    func agentVersion() async throws -> AgentVersion {
        var command = BluetoothCommand()
        command.command = .agentVersion(AgentVersionCommand())
        let response = try await send(command)
        guard case .agentVersion(let r) = response.response else {
            throw WendyError.protocolError("Expected agentVersion response")
        }
        return AgentVersion(
            version: r.version,
            osVersion: r.hasOsVersion ? r.osVersion : nil,
            os: r.os ?? "",
            cpuArchitecture: r.cpuArchitecture ?? "",
            featureset: Array(r.featureset)
        )
    }

    func hardwareCapabilities() async throws -> [HardwareCapability] {
        var command = BluetoothCommand()
        command.command = .hardwareList(HardwareListCommand())
        let response = try await send(command)
        guard case .hardwareList(let r) = response.response else {
            throw WendyError.protocolError("Expected hardwareList response")
        }
        return r.capabilities.map { HardwareCapability(
            category: $0.type,
            devicePath: $0.name,
            details: $0.available ? "available" : "unavailable",
            properties: [:]
        )}
    }
}
```

> **Note on generated optional fields:** swift-protobuf generates `has<FieldName>` helpers for `optional` proto fields and direct property access for non-optional fields. Verify the exact accessor names by inspecting generated files in `.build/` after building.

> **Note on field names in `AppInfo`:** The `AppInfo.state` field is a `string` in `wendy_agent_v1_bluetooth.proto` (not an enum), so we compare against the string `"running"`. The `AppInfo.failureCount` is `int32`, cast to `UInt32`.

- [ ] **Step 2: Verify build**

```bash
swift build
```

Expected: Build succeeds. Fix any generated accessor names as needed by checking `.build/` generated files.

- [ ] **Step 3: Commit**

```bash
git add Sources/WendyCompanionSDK/Transport/BLETransport.swift
git commit -m "feat: add BLE transport with protocol conformances and proto mappings"
```

---

## Task 9: gRPC transport

**Files:**
- Create: `Sources/WendyCompanionSDK/Transport/GRPCTransport.swift`

- [ ] **Step 1: Write GRPCTransport.swift**

> **Important:** The generated gRPC client type names depend on the grpc-swift-protobuf plugin output. After running `swift build`, check the generated `.grpc.swift` files in `.build/` to find the exact client types. Common patterns for grpc-swift 2.x:
> - `WendyContainerService.Client` or `WendyContainerServiceClient`
> - Methods match the proto RPC names in lowerCamelCase
> The code below uses placeholder names — substitute the real ones after inspecting generated output.

```swift
// Sources/WendyCompanionSDK/Transport/GRPCTransport.swift
import GRPCCore
import GRPCNIOTransportHTTP2
import WendyProtos

actor GRPCTransport {
    // The GRPCChannel is used to create per-service clients.
    // It must be run in a background task for the duration of the connection.
    private var channel: GRPCChannel<HTTP2ClientTransport.TransportServices>?
    private var channelTask: Task<Void, Error>?

    func connect(host: String, port: Int) throws {
        let transport = try HTTP2ClientTransport.TransportServices(
            target: .dns(host: host, port: port),
            config: .defaults(transportSecurity: .plaintext)
        )
        let ch = GRPCChannel(transport: transport)
        self.channel = ch
        self.channelTask = Task {
            try await ch.run()
        }
    }

    func close() {
        channelTask?.cancel()
        channelTask = nil
        channel = nil
    }

    private func requireChannel() throws -> GRPCChannel<HTTP2ClientTransport.TransportServices> {
        guard let ch = channel else {
            throw WendyError.connectionFailed("Not connected")
        }
        return ch
    }
}

extension GRPCTransport: AppsTransporting {
    func listApps() async throws -> [App] {
        let ch = try requireChannel()
        // Substitute the real generated client type name here.
        // Example based on grpc-swift 2.x conventions:
        let client = WendyContainerServiceClient(channel: ch)
        var apps: [App] = []
        // ListContainers is a server-streaming RPC returning stream ListContainersResponse
        try await client.listContainers(ListContainersRequest()) { response in
            for try await message in response.messages {
                apps.append(App(
                    appName: message.container.appName,
                    appVersion: message.container.appVersion,
                    runningState: message.container.runningState == .running ? .running : .stopped,
                    failureCount: message.container.failureCount
                ))
            }
        }
        return apps
    }

    func stopApp(named name: String) async throws {
        let ch = try requireChannel()
        let client = WendyContainerServiceClient(channel: ch)
        var request = StopContainerRequest()
        request.appName = name
        _ = try await client.stopContainer(request)
    }

    func removeApp(named name: String, purgeImage: Bool) async throws {
        let ch = try requireChannel()
        let client = WendyContainerServiceClient(channel: ch)
        var request = DeleteContainerRequest()
        request.appName = name
        request.deleteImage = purgeImage
        _ = try await client.deleteContainer(request)
    }

    func startApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        let ch = try requireChannel()
        let client = WendyContainerServiceClient(channel: ch)
        var request = StartContainerRequest()
        request.appName = name
        try await client.startContainer(request) { response in
            for try await message in response.messages {
                switch message.responseType {
                case .started:
                    break
                case .stdoutOutput(let output):
                    try await onOutput(ConsoleOutput(data: output.data, stream: .stdout))
                case .stderrOutput(let output):
                    try await onOutput(ConsoleOutput(data: output.data, stream: .stderr))
                case .none:
                    break
                }
            }
        }
    }

    func attachApp(named name: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        // AttachContainer is a bidirectional streaming RPC:
        //   rpc AttachContainer(stream AttachContainerRequest) returns (stream RunContainerLayersResponse)
        // First message must set app_name; subsequent messages carry stdin_data.
        // The exact grpc-swift 2.x API for bidirectional streaming depends on the generated code.
        // After running `swift build`, find the generated AttachContainer method and use this pattern:
        //
        //   try await client.attachContainer { writer in
        //       var init = AttachContainerRequest()
        //       init.appName = name
        //       try await writer.write(init)
        //       // writer can accept further stdin writes here
        //   } onResponse: { response in
        //       for try await message in response.messages {
        //           switch message.responseType {
        //           case .stdoutOutput(let o): try await onOutput(ConsoleOutput(data: o.data, stream: .stdout))
        //           case .stderrOutput(let o): try await onOutput(ConsoleOutput(data: o.data, stream: .stderr))
        //           default: break
        //           }
        //       }
        //   }
        let ch = try requireChannel()
        let client = WendyContainerServiceClient(channel: ch)
        try await client.attachContainer { writer in
            var initMsg = AttachContainerRequest()
            initMsg.appName = name
            try await writer.write(initMsg)
        } onResponse: { response in
            for try await message in response.messages {
                switch message.responseType {
                case .stdoutOutput(let o):
                    try await onOutput(ConsoleOutput(data: o.data, stream: .stdout))
                case .stderrOutput(let o):
                    try await onOutput(ConsoleOutput(data: o.data, stream: .stderr))
                default:
                    break
                }
            }
        }
    }
}

extension GRPCTransport: WiFiTransporting {
    func listNetworks() async throws -> [WiFiNetwork] {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        let response = try await client.listWiFiNetworks(ListWiFiNetworksRequest())
        return response.networks.map {
            WiFiNetwork(ssid: $0.ssid, signalStrength: $0.hasSignalStrength ? Int($0.signalStrength) : nil)
        }
    }

    func connect(ssid: String, password: String) async throws {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        var request = ConnectToWiFiRequest()
        request.ssid = ssid
        request.password = password
        let response = try await client.connectToWiFi(request)
        if !response.success {
            throw WendyError.deviceError(response.errorMessage ?? "WiFi connect failed")
        }
    }

    func disconnect() async throws {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        let response = try await client.disconnectWiFi(DisconnectWiFiRequest())
        if !response.success {
            throw WendyError.deviceError(response.errorMessage ?? "WiFi disconnect failed")
        }
    }

    func status() async throws -> WiFiStatus {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        let response = try await client.getWiFiStatus(GetWiFiStatusRequest())
        return WiFiStatus(connected: response.connected, ssid: response.hasSsid ? response.ssid : nil)
    }
}

extension GRPCTransport: BluetoothTransporting {
    func listDevices(pairedOnly: Bool) async throws -> [BluetoothDevice] {
        // WendyAgentService.ScanBluetoothPeripherals is a bidirectional streaming RPC.
        // For a companion app use case (one-shot list), this is complex to drive.
        // For now: send one scan request and collect the first response.
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        var devices: [BluetoothDevice] = []
        // The scan RPC streams results; collect until the first batch arrives.
        // Verify the exact generated streaming API against generated code.
        try await client.scanBluetoothPeripherals { requestWriter in
            try await requestWriter.write(ScanBluetoothPeripheralsRequest())
        } onResponse: { response in
            for try await message in response.messages {
                let batch = message.discoveredDevices
                    .filter { !pairedOnly || $0.paired }
                    .map { BluetoothDevice(
                        name: $0.name,
                        address: $0.address,
                        rssi: Int($0.rssi),
                        paired: $0.paired,
                        connected: $0.connected,
                        trusted: $0.trusted,
                        deviceType: $0.deviceType
                    )}
                devices.append(contentsOf: batch)
            }
        }
        return devices
    }

    func connect(address: String) async throws {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        var request = ConnectBluetoothPeripheralRequest()
        request.address = address
        _ = try await client.connectBluetoothPeripheral(request)
    }

    func disconnect(address: String) async throws {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        var request = DisconnectBluetoothPeripheralRequest()
        request.address = address
        _ = try await client.disconnectBluetoothPeripheral(request)
    }

    func forget(address: String) async throws {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        var request = ForgetBluetoothPeripheralRequest()
        request.address = address
        _ = try await client.forgetBluetoothPeripheral(request)
    }
}

extension GRPCTransport: DeviceInfoTransporting {
    func agentVersion() async throws -> AgentVersion {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        let response = try await client.getAgentVersion(GetAgentVersionRequest())
        return AgentVersion(
            version: response.version,
            osVersion: response.hasOsVersion ? response.osVersion : nil,
            os: response.os,
            cpuArchitecture: response.cpuArchitecture,
            featureset: Array(response.featureset)
        )
    }

    func hardwareCapabilities() async throws -> [HardwareCapability] {
        let ch = try requireChannel()
        let client = WendyAgentServiceClient(channel: ch)
        let response = try await client.listHardwareCapabilities(ListHardwareCapabilitiesRequest())
        return response.capabilities.map { HardwareCapability(
            category: $0.category,
            devicePath: $0.devicePath,
            details: $0.description_p,  // 'description' is a reserved keyword; swift-protobuf appends _p
            properties: Dictionary($0.properties.map { ($0.key, $0.value) })
        )}
    }
}

extension GRPCTransport: AudioTransporting {
    func listDevices() async throws -> [AudioDevice] {
        let ch = try requireChannel()
        let client = WendyAudioServiceClient(channel: ch)
        let response = try await client.listAudioDevices(ListAudioDevicesRequest())
        return response.devices.map { AudioDevice(
            id: $0.id,
            name: $0.name,
            details: $0.description_p,
            type: $0.type == .audioDeviceTypeInput ? .input : .output,
            isDefault: $0.isDefault
        )}
    }

    func setDefaultDevice(id: UInt32) async throws {
        let ch = try requireChannel()
        let client = WendyAudioServiceClient(channel: ch)
        var request = SetDefaultAudioDeviceRequest()
        request.deviceID = id
        let response = try await client.setDefaultAudioDevice(request)
        if !response.success {
            throw WendyError.deviceError(response.errorMessage ?? "Set default device failed")
        }
    }

    func streamLevels(deviceID: UInt32, onLevel: (AudioLevel) async throws -> Void) async throws {
        let ch = try requireChannel()
        let client = WendyAudioServiceClient(channel: ch)
        var request = StreamAudioLevelsRequest()
        request.deviceID = deviceID
        try await client.streamAudioLevels(request) { response in
            for try await message in response.messages {
                try await onLevel(AudioLevel(
                    peakDB: message.peakDb,
                    rmsDB: message.rmsDb,
                    timestampNS: message.timestampNs
                ))
            }
        }
    }

    func streamAudio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32, onChunk: (AudioChunk) async throws -> Void) async throws {
        let ch = try requireChannel()
        let client = WendyAudioServiceClient(channel: ch)
        var request = StreamAudioRequest()
        request.deviceID = deviceID
        request.sampleRate = sampleRate
        request.channels = channels
        try await client.streamAudio(request) { response in
            for try await message in response.messages {
                try await onChunk(AudioChunk(
                    pcmData: message.pcmData,
                    timestampNS: message.timestampNs,
                    sampleRate: message.sampleRate,
                    channels: message.channels
                ))
            }
        }
    }
}
```

> **Generated field names:** swift-protobuf renames fields that conflict with Swift keywords. `description` becomes `description_p`. The `AudioDeviceType` enum cases follow the pattern `.audioDeviceTypeInput` / `.audioDeviceTypeOutput` (lowercased proto enum value after removing the enum type prefix). Verify by inspecting generated files.

- [ ] **Step 2: Verify build (may need generated name fixes)**

```bash
swift build 2>&1 | head -40
```

Expected: Build succeeds after fixing any generated type/accessor names. Check `.build/` for generated files if needed.

- [ ] **Step 3: Commit**

```bash
git add Sources/WendyCompanionSDK/Transport/GRPCTransport.swift
git commit -m "feat: add gRPC transport with protocol conformances and proto mappings"
```

---

## Task 10: Device discovery

**Files:**
- Create: `Sources/WendyCompanionSDK/Discovery/DiscoveredDevice.swift`
- Create: `Sources/WendyCompanionSDK/Discovery/DeviceDiscovery.swift`

- [ ] **Step 1: Write DiscoveredDevice.swift**

```swift
// Sources/WendyCompanionSDK/Discovery/DiscoveredDevice.swift
import CoreBluetooth

/// A Wendy device found via BLE scanning.
/// Carries both the CBPeripheral (needed to open the L2CAP channel) and
/// its CBCentralManager (needed to initiate the connection).
public struct DiscoveredDevice: @unchecked Sendable {
    public let name: String
    public let peripheralID: UUID

    // Internal — used by WendyDevice.connect(peripheral:)
    let peripheral: CBPeripheral
    let centralManager: CBCentralManager

    init(peripheral: CBPeripheral, centralManager: CBCentralManager) {
        self.name = peripheral.name ?? "Unknown"
        self.peripheralID = peripheral.identifier
        self.peripheral = peripheral
        self.centralManager = centralManager
    }
}
```

- [ ] **Step 2: Write DeviceDiscovery.swift**

```swift
// Sources/WendyCompanionSDK/Discovery/DeviceDiscovery.swift
import CoreBluetooth

/// Scans for nearby Wendy devices via CoreBluetooth and emits DiscoveredDevice values.
///
/// Usage:
///   let discovery = DeviceDiscovery()
///   for await device in discovery.peripherals {
///       let wendyDevice = try await WendyDevice.connect(peripheral: device)
///   }
///   discovery.stop()
@MainActor
public final class DeviceDiscovery: NSObject {
    private var centralManager: CBCentralManager?
    private var continuation: AsyncStream<DiscoveredDevice>.Continuation?
    private let l2capPSM: CBL2CAPPSM

    public init(l2capPSM: CBL2CAPPSM = WendyBLEConstants.l2capPSM) {
        self.l2capPSM = l2capPSM
        super.init()
    }

    /// An AsyncSequence of discovered Wendy devices.
    /// The sequence completes when `stop()` is called.
    public var peripherals: AsyncStream<DiscoveredDevice> {
        AsyncStream { continuation in
            self.continuation = continuation
            let manager = CBCentralManager(delegate: self, queue: .main)
            self.centralManager = manager
        }
    }

    /// Stops scanning and completes the `peripherals` sequence.
    public func stop() {
        centralManager?.stopScan()
        continuation?.finish()
        continuation = nil
        centralManager = nil
    }
}

extension DeviceDiscovery: CBCentralManagerDelegate {
    public func centralManagerDidUpdateState(_ central: CBCentralManager) {
        guard central.state == .poweredOn else { return }
        central.scanForPeripherals(withServices: [WendyBLEConstants.serviceUUID], options: nil)
    }

    public func centralManager(
        _ central: CBCentralManager,
        didDiscover peripheral: CBPeripheral,
        advertisementData: [String: Any],
        rssi RSSI: NSNumber
    ) {
        let device = DiscoveredDevice(peripheral: peripheral, centralManager: central)
        continuation?.yield(device)
    }
}
```

- [ ] **Step 3: Verify build**

```bash
swift build
```

Expected: Build succeeds.

- [ ] **Step 4: Commit**

```bash
git add Sources/WendyCompanionSDK/Discovery/
git commit -m "feat: add BLE device discovery via CoreBluetooth"
```

---

## Task 11: WendyDevice

**Files:**
- Create: `Sources/WendyCompanionSDK/WendyDevice.swift`

- [ ] **Step 1: Write WendyDevice.swift**

```swift
// Sources/WendyCompanionSDK/WendyDevice.swift
import CoreBluetooth

/// The top-level handle to a connected Wendy device.
/// Obtain via `WendyDevice.connect(peripheral:)` (BLE) or `WendyDevice.connect(host:port:)` (gRPC).
public struct WendyDevice: Sendable {
    public let apps: AppsService
    public let wifi: WiFiService
    public let bluetooth: BluetoothService
    public let info: DeviceInfoService
    public let audio: AudioService

    private let _close: @Sendable () async -> Void

    init(
        apps: AppsService,
        wifi: WiFiService,
        bluetooth: BluetoothService,
        info: DeviceInfoService,
        audio: AudioService,
        close: @escaping @Sendable () async -> Void
    ) {
        self.apps = apps
        self.wifi = wifi
        self.bluetooth = bluetooth
        self.info = info
        self.audio = audio
        self._close = close
    }

    /// Closes the underlying transport connection.
    public func close() async {
        await _close()
    }

    // MARK: - BLE connection

    /// Connects to a discovered Wendy device via BLE L2CAP.
    @MainActor
    public static func connect(peripheral discoveredDevice: DiscoveredDevice, l2capPSM: CBL2CAPPSM = WendyBLEConstants.l2capPSM) async throws -> WendyDevice {
        let result = try await Self.openL2CAPChannel(
            peripheral: discoveredDevice.peripheral,
            centralManager: discoveredDevice.centralManager,
            psm: l2capPSM
        )
        let bleChannel = BLEChannel(channel: result.channel)
        await bleChannel.open()
        let transport = BLETransport(channel: bleChannel)
        return WendyDevice(
            apps: AppsService(transport: transport),
            wifi: WiFiService(transport: transport),
            bluetooth: BluetoothService(transport: transport),
            info: DeviceInfoService(transport: transport),
            audio: AudioService(transport: transport),
            close: { await bleChannel.close() }
        )
    }

    // MARK: - gRPC connection

    /// Connects to a Wendy device over gRPC/TCP.
    public static func connect(host: String, port: Int = WendyDefaults.grpcPort) async throws -> WendyDevice {
        let transport = GRPCTransport()
        try await transport.connect(host: host, port: port)
        return WendyDevice(
            apps: AppsService(transport: transport),
            wifi: WiFiService(transport: transport),
            bluetooth: BluetoothService(transport: transport),
            info: DeviceInfoService(transport: transport),
            audio: AudioService(transport: transport),
            close: { await transport.close() }
        )
    }

    // MARK: - L2CAP helper

    @MainActor
    private static func openL2CAPChannel(
        peripheral: CBPeripheral,
        centralManager: CBCentralManager,
        psm: CBL2CAPPSM
    ) async throws -> (delegate: L2CAPOpenDelegate, channel: CBL2CAPChannel) {
        let delegate = L2CAPOpenDelegate()
        peripheral.delegate = delegate
        centralManager.connect(peripheral, options: nil)
        // [delegate] is captured strongly so it stays alive until the continuation resumes.
        // CBPeripheral.delegate is a weak reference so it wouldn't retain the delegate on its own.
        return try await withCheckedThrowingContinuation { [delegate] continuation in
            delegate.continuation = continuation
            peripheral.openL2CAPChannel(psm)
        }
    }
}

/// Temporary CBPeripheralDelegate used only to open the L2CAP channel.
@MainActor
private final class L2CAPOpenDelegate: NSObject, CBPeripheralDelegate {
    var continuation: CheckedContinuation<(L2CAPOpenDelegate, CBL2CAPChannel), Error>?

    func peripheral(_ peripheral: CBPeripheral, didOpen channel: CBL2CAPChannel?, error: Error?) {
        if let error {
            continuation?.resume(throwing: WendyError.connectionFailed(error.localizedDescription))
        } else if let channel {
            continuation?.resume(returning: (self, channel))
        } else {
            continuation?.resume(throwing: WendyError.connectionFailed("L2CAP channel is nil"))
        }
        continuation = nil
    }
}
```

- [ ] **Step 2: Verify build**

```bash
swift build
```

Expected: Build succeeds.

- [ ] **Step 3: Commit**

```bash
git add Sources/WendyCompanionSDK/WendyDevice.swift
git commit -m "feat: add WendyDevice with BLE and gRPC connection paths"
```

---

## Task 12: AppsService

**Files:**
- Create: `Sources/WendyCompanionSDK/Services/AppsService.swift`
- Create: `Tests/WendyCompanionSDKTests/AppsServiceTests.swift`

- [ ] **Step 1: Write failing tests**

```swift
// Tests/WendyCompanionSDKTests/AppsServiceTests.swift
import Testing
@testable import WendyCompanionSDK

@Suite struct AppsServiceTests {
    @Test func listReturnsAppsFromTransport() async throws {
        var mock = MockAppsTransport()
        mock.stubbedApps = [
            App(appName: "hello", appVersion: "1.0", runningState: .running, failureCount: 0),
            App(appName: "world", appVersion: "2.0", runningState: .stopped, failureCount: 3),
        ]
        let service = AppsService(transport: mock)
        let apps = try await service.list()
        #expect(apps.count == 2)
        #expect(apps[0].appName == "hello")
        #expect(apps[1].runningState == .stopped)
    }

    @Test func stopCallsTransport() async throws {
        let mock = MockAppsTransport()
        let service = AppsService(transport: mock)
        try await service.stop(appName: "hello")  // should not throw
    }

    @Test func stopPropagatesTransportError() async throws {
        var mock = MockAppsTransport()
        mock.stopError = WendyError.deviceError("not found")
        let service = AppsService(transport: mock)
        await #expect(throws: WendyError.self) {
            try await service.stop(appName: "missing")
        }
    }

    @Test func removeCallsTransport() async throws {
        let mock = MockAppsTransport()
        let service = AppsService(transport: mock)
        try await service.remove(appName: "hello", purgeImage: true)
    }

    @Test func startThrowsNotAvailableOnBLEMock() async throws {
        let mock = MockAppsTransport()
        let service = AppsService(transport: mock)
        await #expect(throws: WendyError.self) {
            try await service.startApp(named: "hello") { _ in }
        }
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter AppsServiceTests
```

Expected: FAIL — `AppsService` does not exist.

- [ ] **Step 3: Write AppsService.swift**

```swift
// Sources/WendyCompanionSDK/Services/AppsService.swift
public struct AppsService: Sendable {
    private let transport: any AppsTransporting

    init(transport: any AppsTransporting) {
        self.transport = transport
    }

    public func list() async throws -> [App] {
        try await transport.listApps()
    }

    public func stop(appName: String) async throws {
        try await transport.stopApp(named: appName)
    }

    public func remove(appName: String, purgeImage: Bool) async throws {
        try await transport.removeApp(named: appName, purgeImage: purgeImage)
    }

    public func startApp(named appName: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        try await transport.startApp(named: appName, onOutput: onOutput)
    }

    public func attachApp(named appName: String, onOutput: (ConsoleOutput) async throws -> Void) async throws {
        try await transport.attachApp(named: appName, onOutput: onOutput)
    }
}
```

- [ ] **Step 4: Run tests to confirm pass**

```bash
swift test --filter AppsServiceTests
```

Expected: All 5 tests pass.

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyCompanionSDK/Services/AppsService.swift
git add Tests/WendyCompanionSDKTests/AppsServiceTests.swift
git commit -m "feat: add AppsService"
```

---

## Task 13: WiFiService

**Files:**
- Create: `Sources/WendyCompanionSDK/Services/WiFiService.swift`
- Create: `Tests/WendyCompanionSDKTests/WiFiServiceTests.swift`

- [ ] **Step 1: Write failing tests**

```swift
// Tests/WendyCompanionSDKTests/WiFiServiceTests.swift
import Testing
@testable import WendyCompanionSDK

@Suite struct WiFiServiceTests {
    @Test func listNetworksReturnsFromTransport() async throws {
        var mock = MockWiFiTransport()
        mock.stubbedNetworks = [
            WiFiNetwork(ssid: "HomeNet", signalStrength: -60),
            WiFiNetwork(ssid: "GuestNet", signalStrength: nil),
        ]
        let service = WiFiService(transport: mock)
        let networks = try await service.listNetworks()
        #expect(networks.count == 2)
        #expect(networks[0].ssid == "HomeNet")
        #expect(networks[1].signalStrength == nil)
    }

    @Test func statusReturnsFromTransport() async throws {
        var mock = MockWiFiTransport()
        mock.stubbedStatus = WiFiStatus(connected: true, ssid: "HomeNet")
        let service = WiFiService(transport: mock)
        let status = try await service.status()
        #expect(status.connected == true)
        #expect(status.ssid == "HomeNet")
    }

    @Test func connectCallsTransport() async throws {
        let mock = MockWiFiTransport()
        let service = WiFiService(transport: mock)
        try await service.connect(ssid: "HomeNet", password: "secret")
    }

    @Test func disconnectCallsTransport() async throws {
        let mock = MockWiFiTransport()
        let service = WiFiService(transport: mock)
        try await service.disconnect()
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter WiFiServiceTests
```

Expected: FAIL — `WiFiService` does not exist.

- [ ] **Step 3: Write WiFiService.swift**

```swift
// Sources/WendyCompanionSDK/Services/WiFiService.swift
public struct WiFiService: Sendable {
    private let transport: any WiFiTransporting

    init(transport: any WiFiTransporting) {
        self.transport = transport
    }

    public func listNetworks() async throws -> [WiFiNetwork] {
        try await transport.listNetworks()
    }

    public func connect(ssid: String, password: String) async throws {
        try await transport.connect(ssid: ssid, password: password)
    }

    public func disconnect() async throws {
        try await transport.disconnect()
    }

    public func status() async throws -> WiFiStatus {
        try await transport.status()
    }
}
```

- [ ] **Step 4: Run tests to confirm pass**

```bash
swift test --filter WiFiServiceTests
```

Expected: All 4 tests pass.

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyCompanionSDK/Services/WiFiService.swift
git add Tests/WendyCompanionSDKTests/WiFiServiceTests.swift
git commit -m "feat: add WiFiService"
```

---

## Task 14: BluetoothService

**Files:**
- Create: `Sources/WendyCompanionSDK/Services/BluetoothService.swift`
- Create: `Tests/WendyCompanionSDKTests/BluetoothServiceTests.swift`

- [ ] **Step 1: Write failing tests**

```swift
// Tests/WendyCompanionSDKTests/BluetoothServiceTests.swift
import Testing
@testable import WendyCompanionSDK

@Suite struct BluetoothServiceTests {
    private func makeDevice(paired: Bool = false) -> BluetoothDevice {
        BluetoothDevice(name: "Speaker", address: "AA:BB:CC", rssi: -70, paired: paired, connected: false, trusted: false, deviceType: "audio-card")
    }

    @Test func listAllDevices() async throws {
        var mock = MockBluetoothTransport()
        mock.stubbedDevices = [makeDevice(paired: false), makeDevice(paired: true)]
        let service = BluetoothService(transport: mock)
        let devices = try await service.list(pairedOnly: false)
        #expect(devices.count == 2)
    }

    @Test func listPairedOnly() async throws {
        var mock = MockBluetoothTransport()
        mock.stubbedDevices = [makeDevice(paired: false), makeDevice(paired: true)]
        let service = BluetoothService(transport: mock)
        let devices = try await service.list(pairedOnly: true)
        #expect(devices.count == 1)
        #expect(devices[0].paired == true)
    }

    @Test func connectCallsTransport() async throws {
        let mock = MockBluetoothTransport()
        let service = BluetoothService(transport: mock)
        try await service.connect(address: "AA:BB:CC")
    }

    @Test func disconnectCallsTransport() async throws {
        let mock = MockBluetoothTransport()
        let service = BluetoothService(transport: mock)
        try await service.disconnect(address: "AA:BB:CC")
    }

    @Test func forgetCallsTransport() async throws {
        let mock = MockBluetoothTransport()
        let service = BluetoothService(transport: mock)
        try await service.forget(address: "AA:BB:CC")
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter BluetoothServiceTests
```

Expected: FAIL — `BluetoothService` does not exist.

- [ ] **Step 3: Write BluetoothService.swift**

```swift
// Sources/WendyCompanionSDK/Services/BluetoothService.swift
public struct BluetoothService: Sendable {
    private let transport: any BluetoothTransporting

    init(transport: any BluetoothTransporting) {
        self.transport = transport
    }

    /// Lists Bluetooth peripherals on the Wendy device (not the iOS device).
    public func list(pairedOnly: Bool = false) async throws -> [BluetoothDevice] {
        try await transport.listDevices(pairedOnly: pairedOnly)
    }

    public func connect(address: String) async throws {
        try await transport.connect(address: address)
    }

    public func disconnect(address: String) async throws {
        try await transport.disconnect(address: address)
    }

    public func forget(address: String) async throws {
        try await transport.forget(address: address)
    }
}
```

- [ ] **Step 4: Run tests to confirm pass**

```bash
swift test --filter BluetoothServiceTests
```

Expected: All 5 tests pass.

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyCompanionSDK/Services/BluetoothService.swift
git add Tests/WendyCompanionSDKTests/BluetoothServiceTests.swift
git commit -m "feat: add BluetoothService"
```

---

## Task 15: DeviceInfoService

**Files:**
- Create: `Sources/WendyCompanionSDK/Services/DeviceInfoService.swift`
- Create: `Tests/WendyCompanionSDKTests/DeviceInfoServiceTests.swift`

- [ ] **Step 1: Write failing tests**

```swift
// Tests/WendyCompanionSDKTests/DeviceInfoServiceTests.swift
import Testing
@testable import WendyCompanionSDK

@Suite struct DeviceInfoServiceTests {
    @Test func agentVersionReturnsFromTransport() async throws {
        var mock = MockDeviceInfoTransport()
        mock.stubbedVersion = AgentVersion(
            version: "2.3.1",
            osVersion: "1.4.0",
            os: "wendyos",
            cpuArchitecture: "aarch64",
            featureset: ["wifi", "bluetooth"]
        )
        let service = DeviceInfoService(transport: mock)
        let version = try await service.agentVersion()
        #expect(version.version == "2.3.1")
        #expect(version.featureset.contains("wifi"))
    }

    @Test func hardwareCapabilitiesReturnsFromTransport() async throws {
        var mock = MockDeviceInfoTransport()
        mock.stubbedCapabilities = [
            HardwareCapability(category: "gpu", devicePath: "/dev/video0", details: "Camera", properties: [:])
        ]
        let service = DeviceInfoService(transport: mock)
        let caps = try await service.hardwareCapabilities()
        #expect(caps.count == 1)
        #expect(caps[0].category == "gpu")
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter DeviceInfoServiceTests
```

Expected: FAIL — `DeviceInfoService` does not exist.

- [ ] **Step 3: Write DeviceInfoService.swift**

```swift
// Sources/WendyCompanionSDK/Services/DeviceInfoService.swift
public struct DeviceInfoService: Sendable {
    private let transport: any DeviceInfoTransporting

    init(transport: any DeviceInfoTransporting) {
        self.transport = transport
    }

    public func agentVersion() async throws -> AgentVersion {
        try await transport.agentVersion()
    }

    public func hardwareCapabilities() async throws -> [HardwareCapability] {
        try await transport.hardwareCapabilities()
    }
}
```

- [ ] **Step 4: Run tests to confirm pass**

```bash
swift test --filter DeviceInfoServiceTests
```

Expected: All 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add Sources/WendyCompanionSDK/Services/DeviceInfoService.swift
git add Tests/WendyCompanionSDKTests/DeviceInfoServiceTests.swift
git commit -m "feat: add DeviceInfoService"
```

---

## Task 16: AudioService

**Files:**
- Create: `Sources/WendyCompanionSDK/Services/AudioService.swift`
- Create: `Tests/WendyCompanionSDKTests/AudioServiceTests.swift`

- [ ] **Step 1: Write failing tests**

```swift
// Tests/WendyCompanionSDKTests/AudioServiceTests.swift
import Testing
@testable import WendyCompanionSDK

@Suite struct AudioServiceTests {
    @Test func listDevicesReturnsFromTransport() async throws {
        var mock = MockAudioTransport()
        mock.stubbedDevices = [
            AudioDevice(id: 1, name: "Built-in Mic", details: "Default input", type: .input, isDefault: true),
            AudioDevice(id: 2, name: "HDMI Audio", details: "HDMI output", type: .output, isDefault: false),
        ]
        let service = AudioService(transport: mock)
        let devices = try await service.listDevices()
        #expect(devices.count == 2)
        #expect(devices[0].type == .input)
        #expect(devices[0].isDefault == true)
    }

    @Test func setDefaultDeviceCallsTransport() async throws {
        let mock = MockAudioTransport()
        let service = AudioService(transport: mock)
        try await service.setDefaultDevice(id: 1)
    }

    @Test func streamLevelsCallsOnLevelClosure() async throws {
        struct CountingAudioTransport: AudioTransporting {
            func listDevices() async throws -> [AudioDevice] { [] }
            func setDefaultDevice(id: UInt32) async throws {}
            func streamLevels(deviceID: UInt32, onLevel: (AudioLevel) async throws -> Void) async throws {
                try await onLevel(AudioLevel(peakDB: -3.0, rmsDB: -6.0, timestampNS: 1000))
                try await onLevel(AudioLevel(peakDB: -5.0, rmsDB: -8.0, timestampNS: 2000))
            }
            func streamAudio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32, onChunk: (AudioChunk) async throws -> Void) async throws {}
        }

        var received: [AudioLevel] = []
        let service = AudioService(transport: CountingAudioTransport())
        try await service.streamLevels(deviceID: 0) { level in
            received.append(level)
        }
        #expect(received.count == 2)
        #expect(received[0].peakDB == -3.0)
    }
}
```

- [ ] **Step 2: Run test to confirm failure**

```bash
swift test --filter AudioServiceTests
```

Expected: FAIL — `AudioService` does not exist.

- [ ] **Step 3: Write AudioService.swift**

```swift
// Sources/WendyCompanionSDK/Services/AudioService.swift
public struct AudioService: Sendable {
    private let transport: any AudioTransporting

    init(transport: any AudioTransporting) {
        self.transport = transport
    }

    public func listDevices() async throws -> [AudioDevice] {
        try await transport.listDevices()
    }

    public func setDefaultDevice(id: UInt32) async throws {
        try await transport.setDefaultDevice(id: id)
    }

    /// Streams real-time audio levels. Calls `onLevel` for each update until cancelled or an error occurs.
    public func streamLevels(deviceID: UInt32 = 0, onLevel: (AudioLevel) async throws -> Void) async throws {
        try await transport.streamLevels(deviceID: deviceID, onLevel: onLevel)
    }

    /// Streams raw PCM audio. Calls `onChunk` for each audio chunk until cancelled or an error occurs.
    public func streamAudio(deviceID: UInt32 = 0, sampleRate: UInt32 = 48000, channels: UInt32 = 1, onChunk: (AudioChunk) async throws -> Void) async throws {
        try await transport.streamAudio(deviceID: deviceID, sampleRate: sampleRate, channels: channels, onChunk: onChunk)
    }
}
```

- [ ] **Step 4: Run tests to confirm pass**

```bash
swift test --filter AudioServiceTests
```

Expected: All 3 tests pass.

- [ ] **Step 5: Run full test suite**

```bash
swift test
```

Expected: All tests pass.

- [ ] **Step 6: Commit**

```bash
git add Sources/WendyCompanionSDK/Services/AudioService.swift
git add Tests/WendyCompanionSDKTests/AudioServiceTests.swift
git commit -m "feat: add AudioService"
```

---

## Post-implementation notes

### Build plugin verification (Task 2)
If `grpc-swift-protobuf.json` config format does not match the installed plugin version, check:
- The plugin README inside `.build/checkouts/grpc-swift-protobuf/`
- Any `ERROR: Plugin 'GRPCProtobufGenerator'` messages from `swift build`

### Generated type names (Tasks 8, 9)
After the first successful build, run:
```bash
find .build/plugins -name "*.pb.swift" -o -name "*.grpc.swift" 2>/dev/null | head -10
```
Open a generated file to confirm message type names and gRPC client names, then fix any mismatches in `BLETransport.swift` and `GRPCTransport.swift`.

### Bluetooth scan over gRPC (Task 9)
`ScanBluetoothPeripherals` is a bidirectional streaming RPC. The `GRPCTransport.listDevices` implementation uses a one-shot approach. If the generated client API for bidirectional streaming differs, adjust the implementation — the `BluetoothTransporting` protocol signature stays the same.

### L2CAP PSM (Task 3 / Task 10)
Update `WendyBLEConstants.l2capPSM` in `Constants.swift` when the PSM is finalized on the agent side.
