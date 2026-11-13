import Foundation
import GRPCCore
import Synchronization
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("USB arm metadata and bindings")
struct RobotArmTests {
    typealias Device = Wendy_Agent_Services_V2_RobotArm

    private func metadata(
        serial: String = "A",
        path: String = "/dev/cu.fixture",
        location: String = "1257472",
        pid: String = "21971"
    ) -> [String: String] {
        [
            "idVendor": "6790", "idProduct": pid, "locationID": location,
            "bInterfaceNumber": "1", "IOCalloutDevice": path, "USB Serial Number": serial,
            "USB Vendor Name": "WCH", "USB Product Name": "USB serial",
        ]
    }

    @Test("Mac metadata joins USB identity, excludes consoles, and never verifies an arm")
    func classification() throws {
        let d = try #require(USBSerialInventory.device(metadata()))
        #expect(d.id == "usb:1a86:55d3:s:41:1")
        #expect(d.candidate && d.connected)
        #expect(!d.configured && !d.verified && d.model.isEmpty)
        #expect(d.accessStatus == "unknown")  // Busy, inaccessible and unpowered are not probed.
        #expect(d.usbLocation == "00133000")
        #expect(d.manufacturer == "WCH")
        #expect(USBSerialInventory.device(["IOCalloutDevice": "/dev/cu.debug-console"]) == nil)
        #expect(
            USBSerialInventory.device(["IOCalloutDevice": "/dev/cu.Bluetooth-Incoming-Port"]) == nil
        )
        var unknown = metadata()
        unknown["idVendor"] = "1027"
        unknown["idProduct"] = "24577"
        #expect(try #require(USBSerialInventory.device(unknown)).candidate == false)
        #expect(USBSerialInventory.device(metadata(path: "/dev/tty.fixture")) == nil)
    }

    @Test(
        "Serial identity survives node renumbering and socket changes; missing serial follows location"
    )
    func identities() throws {
        let original = try #require(USBSerialInventory.device(metadata()))
        let moved = try #require(
            USBSerialInventory.device(metadata(path: "/dev/cu.renumbered", location: "2000000"))
        )
        #expect(original.id == moved.id)
        let weak1 = try #require(USBSerialInventory.device(metadata(serial: "")))
        let weak2 = try #require(
            USBSerialInventory.device(metadata(serial: "", location: "2000000"))
        )
        #expect(weak1.identityKind == "usb_location")
        #expect(weak1.id != weak2.id)
        #expect(weak1.note.contains("replacement"))
        let duplicates = RobotArmService.merge(live: [original, moved], saved: [])
        #expect(duplicates.count == 2)
        #expect(duplicates.allSatisfy { !$0.connected && $0.connectionStatus == "ambiguous" })
        var second = metadata(serial: "B")
        second["bInterfaceNumber"] = "0"
        let another = try #require(USBSerialInventory.device(second))
        #expect(
            RobotArmService.merge(live: [original, another], saved: []).allSatisfy { $0.connected }
        )
    }

    @Test("Target-local binding persists, follows replug and can be forgotten offline")
    func lifecycle() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let path = root.appendingPathComponent("agent/robot-arm-bindings.json")
        let d = try #require(USBSerialInventory.device(metadata()))
        let live = Mutex([d])
        let service = RobotArmService(path: path, discover: { live.withLock { $0 } })
        var request = Wendy_Agent_Services_V2_BindRobotArmRequest()
        request.id = d.id
        request.model = "so101"
        request.name = "Follower"
        let bound = try await service.bind(request)
        #expect(bound.configured && !bound.verified)
        #expect(bound.name == "Follower")
        var unnamed = request
        unnamed.name = ""
        #expect(try await service.bind(unnamed).name == "Follower")
        let permissions =
            try FileManager.default.attributesOfItem(atPath: path.path)[.posixPermissions]
            as? NSNumber
        #expect(permissions?.intValue == 0o600)
        let moved = try #require(
            USBSerialInventory.device(metadata(path: "/dev/cu.replugged", location: "2000000"))
        )
        live.withLock { $0 = [moved] }
        let current = try #require(try await service.devices().first)
        #expect(current.devicePath == "/dev/cu.replugged" && current.name == "Follower")
        live.withLock { $0 = [] }
        let restarted = RobotArmService(path: path, discover: { [] })
        let offline = try #require(try await restarted.devices().first)
        #expect(!offline.connected && offline.configured)
        #expect(offline.devicePath.isEmpty && offline.connectionStatus == "disconnected")
        await #expect(throws: RPCError.self) { try await service.bind(request) }
        try await restarted.forget(d.id)
        #expect(try await service.devices().isEmpty)
    }

    @Test("Weak identity needs acknowledgement; duplicate or disconnected endpoints cannot bind")
    func bindingChecks() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let d = try #require(USBSerialInventory.device(metadata(serial: "")))
        let live = Mutex([d])
        let service = RobotArmService(
            path: root.appendingPathComponent("bindings.json"),
            discover: { live.withLock { $0 } }
        )
        var request = Wendy_Agent_Services_V2_BindRobotArmRequest()
        request.id = d.id
        request.model = "so101"
        await #expect(throws: RPCError.self) { try await service.bind(request) }
        request.allowLocationBinding = true
        #expect(try await service.bind(request).configured)
        live.withLock { $0 = [d, d] }
        await #expect(throws: RPCError.self) { try await service.bind(request) }
        request.model = "unsupported"
        await #expect(throws: RPCError.self) { try await service.bind(request) }
        await #expect(throws: RPCError.self) { try await service.forget("") }
    }

    @Test("Corrupt persistence fails closed instead of discarding saved records")
    func corruptState() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let path = root.appendingPathComponent("bindings.json")
        try Data("broken".utf8).write(to: path)
        let service = RobotArmService(path: path, discover: { [] })
        await #expect(throws: RPCError.self) { try await service.forget("any") }
        #expect(try String(contentsOf: path, encoding: .utf8) == "broken")
    }

    @Test("Fields written by a newer Agent do not make saved bindings unreadable")
    func unknownFields() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let path = root.appendingPathComponent("bindings.json")
        let saved = #"[{"id":"x","model":"so101","configured":true,"name":"F","newer":1}]"#
        try Data(saved.utf8).write(to: path)
        let service = RobotArmService(path: path, discover: { [] })
        #expect(try await service.devices().first?.name == "F")
    }

    @Test("RPC handlers keep discovery and saved bindings on the target Agent")
    func rpcHandlers() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let device = try #require(USBSerialInventory.device(metadata(path: "/dev/cu.target-agent")))
        let service = RobotArmService(
            path: root.appendingPathComponent("target/bindings.json"),
            discover: { [device] }
        )
        let context = ServerContext(
            descriptor: MethodDescriptor(
                fullyQualifiedService: "wendy.agent.services.v2.WendyRobotArmService",
                method: "ListRobotArms"
            ),
            remotePeer: "in-process:client-on-another-host",
            localPeer: "in-process:target",
            cancellation: .init()
        )
        let listed = try await service.listRobotArms(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V2_ListRobotArmsRequest()
            ),
            context: context
        )
        #expect(try listed.message.devices.first?.devicePath == "/dev/cu.target-agent")
        var bind = Wendy_Agent_Services_V2_BindRobotArmRequest()
        bind.id = device.id
        bind.model = "so101"
        let bound = try await service.bindRobotArm(
            request: ServerRequest(metadata: [:], message: bind),
            context: context
        )
        #expect(try bound.message.device.configured)
        #expect(try !bound.message.device.verified)
        var forget = Wendy_Agent_Services_V2_ForgetRobotArmRequest()
        forget.id = device.id
        _ = try await service.forgetRobotArm(
            request: ServerRequest(metadata: [:], message: forget),
            context: context
        )
        #expect(try await service.devices().first?.configured == false)
    }

    @Test(
        "Read-only live Mac USB metadata smoke",
        .enabled(if: ProcessInfo.processInfo.environment["WENDY_USB_METADATA_SMOKE"] == "1")
    )
    func liveMacMetadata() throws {
        let devices = try USBSerialInventory().discover()
        #expect(
            devices.allSatisfy {
                !$0.vendorID.isEmpty && !$0.productID.isEmpty && !$0.verified
                    && $0.accessStatus == "unknown"
            }
        )
        for d in devices {
            print("USB metadata: \(d.id) \(d.devicePath) \(d.product) candidate=\(d.candidate)")
        }
    }
}
