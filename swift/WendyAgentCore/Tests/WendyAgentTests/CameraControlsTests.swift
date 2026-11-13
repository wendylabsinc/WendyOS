import Foundation
import Synchronization
import Testing

@testable import WendyAgentCore

@Suite("Mac UVC camera controls")
struct CameraControlsTests {
    @Test
    func `USB identity is exact and malformed identities fail closed`() throws {
        let identity = try UVCIdentity(uniqueID: "0x1000000c456366")
        #expect(identity.location == 0x100000)
        #expect(identity.vendor == 0x0c45)
        #expect(identity.product == 0x6366)
        for uid in [
            "USB Camera", "built-in", "0x0c456366", "0x10000000000000", "0x1000000c45636600ff",
            "0xzz",
        ] {
            #expect(throws: (any Error).self) { try UVCIdentity(uniqueID: uid) }
        }
    }

    @Test
    func `descriptor bitmap gates standard controls and ignores vendor units`() throws {
        let definitions = try UVCDescriptor.controls(controlDescriptor())
        #expect(
            definitions.map(\.name) == [
                "auto_exposure", "brightness", "exposure_time_absolute", "gain",
            ]
        )
        #expect(definitions.first { $0.name == "gain" }?.entity == 2)
        #expect(definitions.first { $0.name == "exposure_time_absolute" }?.entity == 1)
        for count in 0..<controlDescriptor().count {
            #expect(throws: (any Error).self) {
                try UVCDescriptor.controls(Array(controlDescriptor().prefix(count)))
            }
        }
        var bad = controlDescriptor()
        bad[9] = 0
        #expect(throws: (any Error).self) { try UVCDescriptor.controls(bad) }
        var ambiguous = controlDescriptor()
        ambiguous += Array(controlDescriptor()[18..<36])
        ambiguous[2] = UInt8(ambiguous.count)
        #expect(throws: (any Error).self) { try UVCDescriptor.controls(ambiguous) }
    }

    @Test
    func `reads real ranges signed brightness and sparse V4L2 exposure modes`() throws {
        let transport = FakeUVCTransport()
        let session = try UVCControlSession(transport: transport)
        let controls = try session.definitions.map(session.read)
        let mode = try #require(controls.first { $0.name == "auto_exposure" })
        #expect(mode.value == 3 && mode.defaultValue == 3)
        #expect(mode.minimum == 1 && mode.maximum == 3 && mode.supportedModes == 9)
        let brightness = try #require(controls.first { $0.name == "brightness" })
        #expect(brightness.minimum == -64 && brightness.maximum == 64)
        let exposure = try #require(controls.first { $0.name == "exposure_time_absolute" })
        #expect(exposure.value == 156 && exposure.minimum == 1 && exposure.maximum == 5000)
        #expect(!exposure.mutable)
        #expect(transport.state.withLock { $0.writes.isEmpty })
    }

    @Test
    func `validates modes bounds inactive controls and transfer readback before success`() throws {
        let transport = FakeUVCTransport()
        let session = try UVCControlSession(transport: transport)
        for (name, value) in [
            ("auto_exposure", Int32(0)), ("auto_exposure", 2), ("gain", 101), ("gain", -1),
            ("unknown", 1), ("exposure_time_absolute", 20),
        ] {
            #expect(throws: (any Error).self) { try session.write(name: name, value: value) }
        }
        #expect(transport.state.withLock { $0.writes.isEmpty })
        try session.write(name: "auto_exposure", value: 1)
        try session.write(name: "exposure_time_absolute", value: 20)
        #expect(transport.state.withLock { $0.mode == 1 && $0.exposure == 20 })
        transport.state.withLock { $0.ignoreGainWrites = true }
        #expect(throws: (any Error).self) { try session.write(name: "gain", value: 10) }
        transport.state.withLock { $0.shortInfo = true }
        #expect(throws: (any Error).self) { try session.read(session.definitions[0]) }
    }

    @Test
    func
        `applies auto modes first persists only successful controls and restores across instances`()
        async throws
    {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appendingPathComponent("controls.json")
        let transport = FakeUVCTransport()
        let backend = CameraControls(storeURL: url, open: { _ in transport })
        let uid = "0x1000000c456366"
        let outcomes = try await backend.set(
            uniqueID: uid,
            values: [("exposure_time_absolute", 20), ("auto_exposure", 1), ("gain", 101)],
            persist: true
        )
        #expect(outcomes.map(\.name) == ["auto_exposure", "exposure_time_absolute", "gain"])
        #expect(outcomes.map(\.applied) == [true, true, false])
        transport.state.withLock {
            $0.mode = 8
            $0.exposure = 156
            $0.writes = []
        }
        let restarted = CameraControls(storeURL: url, open: { _ in transport })
        try await restarted.restore(uniqueID: uid)
        #expect(transport.state.withLock { $0.mode == 1 && $0.exposure == 20 })
        #expect(
            transport.state.withLock {
                $0.writes.map(\.0) == ["auto_exposure", "exposure_time_absolute"]
            }
        )
        let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
        #expect((attributes[.posixPermissions] as? NSNumber)?.intValue == 0o600)
        let results = try await restarted.reset(uniqueID: uid, names: [])
        #expect(results.count == 2 && results.allSatisfy(\.applied))
        #expect(transport.state.withLock { $0.mode == 8 && $0.exposure == 156 })
        transport.state.withLock { $0.writes = [] }
        let afterReset = CameraControls(storeURL: url, open: { _ in transport })
        try await afterReset.restore(uniqueID: uid)
        #expect(transport.state.withLock { $0.writes.isEmpty })
    }

    @Test
    func `once-only settings do not create persisted overrides and inactive reset still forgets`()
        async throws
    {
        let transport = FakeUVCTransport()
        let backend = CameraControls(storeURL: nil, open: { _ in transport })
        let uid = "0x1000000c456366"
        _ = try await backend.set(uniqueID: uid, values: [("gain", 10)], persist: false)
        transport.state.withLock { $0.writes = [] }
        try await backend.restore(uniqueID: uid)
        #expect(transport.state.withLock { $0.writes.isEmpty })
        _ = try await backend.set(
            uniqueID: uid,
            values: [("auto_exposure", 1), ("exposure_time_absolute", 20)],
            persist: true
        )
        _ = try await backend.set(uniqueID: uid, values: [("auto_exposure", 3)], persist: false)
        let result = try await backend.reset(uniqueID: uid, names: ["exposure_time_absolute"])
        #expect(result.count == 1 && !result[0].applied)
        transport.state.withLock { $0.writes = [] }
        try await backend.restore(uniqueID: uid)
        #expect(transport.state.withLock { $0.writes.map(\.0) == ["auto_exposure"] })
    }

    @Test
    func `inactive defaults require no write and disconnected reset still removes persistence`()
        async throws
    {
        let transport = FakeUVCTransport()
        let unavailable = Mutex(false)
        let backend = CameraControls(
            storeURL: nil,
            open: { _ in
                if unavailable.withLock({ $0 }) { throw UVCError.transfer(-1) }
                return transport
            }
        )
        let uid = "0x1000000c456366"
        let alreadyDefault = try await backend.reset(
            uniqueID: uid,
            names: ["exposure_time_absolute"]
        )
        #expect(alreadyDefault.first?.applied == true)
        #expect(transport.state.withLock { $0.writes.isEmpty })
        _ = try await backend.set(uniqueID: uid, values: [("gain", 10)], persist: true)
        unavailable.withLock { $0 = true }
        await #expect(throws: (any Error).self) {
            try await backend.reset(uniqueID: uid, names: [])
        }
        unavailable.withLock { $0 = false }
        transport.state.withLock { $0.writes = [] }
        try await backend.restore(uniqueID: uid)
        #expect(transport.state.withLock { $0.writes.isEmpty })
    }

    @Test
    func `duplicate writes and symlinked persistence fail before USB mutations`() async throws {
        let transport = FakeUVCTransport()
        let backend = CameraControls(storeURL: nil, open: { _ in transport })
        await #expect(throws: (any Error).self) {
            try await backend.set(
                uniqueID: "0x1000000c456366",
                values: [("gain", 1), ("gain", 2)],
                persist: true
            )
        }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let target = directory.appendingPathComponent("target.json")
        try Data("{}".utf8).write(to: target)
        let link = directory.appendingPathComponent("link.json")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: target)
        let unsafeStore = CameraControls(storeURL: link, open: { _ in transport })
        await #expect(throws: (any Error).self) {
            try await unsafeStore.set(
                uniqueID: "0x1000000c456366",
                values: [("gain", 1)],
                persist: true
            )
        }
        #expect(transport.state.withLock { $0.writes.isEmpty })
    }
}

private func controlDescriptor() -> [UInt8] {
    var bytes: [UInt8] = [9, 2, 0, 0, 1, 1, 0, 0x80, 0]
    bytes += [9, 4, 0, 0, 0, 14, 1, 0, 0]
    bytes += [18, 0x24, 2, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0x0a, 0, 0]
    bytes += [11, 0x24, 5, 2, 1, 0, 0, 2, 1, 2, 0]
    bytes[2] = UInt8(bytes.count)
    return bytes
}

private final class FakeUVCTransport: UVCControlTransport, Sendable {
    struct State {
        var mode: Int32 = 8
        var exposure: Int32 = 156
        var gain: Int32 = 0
        var brightness: Int32 = 0
        var writes: [(String, Int32)] = []
        var ignoreGainWrites = false
        var shortInfo = false
    }
    let state = Mutex(State())
    func configuration() throws -> [UInt8] { controlDescriptor() }
    func request(_ d: UVCControlDefinition, request: UInt8, bytes: [UInt8]) throws -> [UInt8] {
        state.withLock { state in
            if request == 0x86 {
                if state.shortInfo { return [] }
                return [d.name == "exposure_time_absolute" && state.mode != 1 ? 15 : 3]
            }
            let value: Int32
            if request == 1 {
                let bits = bytes.enumerated().reduce(UInt32(0)) {
                    $0 | UInt32($1.element) << (8 * $1.offset)
                }
                value = Int32(bitPattern: bits)
                state.writes.append((d.name, value))
                switch d.name {
                case "auto_exposure": state.mode = value
                case "exposure_time_absolute": state.exposure = value
                case "gain": if !state.ignoreGainWrites { state.gain = value }
                default: state.brightness = value
                }
                return bytes
            }
            switch (d.name, request) {
            case ("auto_exposure", 0x81): value = state.mode
            case ("auto_exposure", 0x87): value = 8
            case ("auto_exposure", 0x84): value = 9
            case ("exposure_time_absolute", 0x81): value = state.exposure
            case ("exposure_time_absolute", 0x87): value = 156
            case ("exposure_time_absolute", 0x83): value = 5000
            case ("exposure_time_absolute", 0x82): value = 1
            case ("gain", 0x81): value = state.gain
            case ("gain", 0x83): value = 100
            case ("brightness", 0x81): value = state.brightness
            case ("brightness", 0x82): value = -64
            case ("brightness", 0x83): value = 64
            case (_, 0x84): value = 1
            default: value = 0
            }
            return (0..<d.length).map {
                UInt8(truncatingIfNeeded: UInt32(bitPattern: value) >> (8 * $0))
            }
        }
    }
}
