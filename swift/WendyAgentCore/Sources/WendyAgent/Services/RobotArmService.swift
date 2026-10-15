import Foundation
import GRPCCore
import SwiftProtobuf
import WendyAgentGRPC

/// Host-local configuration only. No serial opens, probing, driver provisioning,
/// calibration or control. Registered on the main Agent transport.
actor RobotArmService: Wendy_Agent_Services_V2_WendyRobotArmService.ServiceProtocol {
    typealias Device = Wendy_Agent_Services_V2_RobotArm
    let path: URL
    let discover: @Sendable () throws -> [Device]

    init(
        path: URL,
        discover: @escaping @Sendable () throws -> [Device] = {
            try USBSerialInventory().discover()
        }
    ) {
        self.path = path
        self.discover = discover
    }

    private func saved() throws -> [Device] {
        let data: Data
        do { data = try Data(contentsOf: path) } catch CocoaError.fileReadNoSuchFile { return [] }
        guard let objects = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else {
            throw RPCError(code: .internalError, message: "Invalid robot arm binding store")
        }
        let devices = try objects.map {
            try Device(jsonUTF8Data: JSONSerialization.data(withJSONObject: $0))
        }
        guard devices.allSatisfy({ !$0.id.isEmpty && $0.model == "so101" && $0.configured }),
            Set(devices.map(\.id)).count == devices.count
        else { throw RPCError(code: .internalError, message: "Invalid robot arm binding store") }
        return devices
    }

    private func save(_ devices: [Device]) throws {
        let objects = try devices.sorted { $0.id < $1.id }.map { device in
            var record = device
            record.connected = false
            record.verified = false
            record.devicePath = ""
            record.connectionStatus = ""
            record.accessStatus = ""
            return try JSONSerialization.jsonObject(with: record.jsonUTF8Data())
        }
        let data = try JSONSerialization.data(
            withJSONObject: objects,
            options: [.prettyPrinted, .sortedKeys]
        )
        try FileManager.default.createDirectory(
            at: path.deletingLastPathComponent(),
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        try data.write(to: path, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path.path)
    }

    static func merge(live: [Device], saved: [Device]) -> [Device] {
        var groups = Dictionary(grouping: live, by: \.id)
        for record in saved {
            guard var matches = groups[record.id] else {
                var d = record
                d.connected = false
                d.verified = false
                d.devicePath = ""
                d.connectionStatus = "disconnected"
                d.accessStatus = "unknown"
                groups[record.id] = [d]
                continue
            }
            for i in matches.indices {
                matches[i].name = record.name
                matches[i].model = record.model
                matches[i].configured = true
                matches[i].verified = false
            }
            groups[record.id] = matches
        }
        var result: [Device] = []
        for group in groups.values {
            for var device in group {
                if group.count > 1 {
                    device.connected = false
                    device.connectionStatus = "ambiguous"
                    device.note =
                        "Multiple endpoints share this identity; binding is blocked. Disconnect duplicates before binding."
                }
                result.append(device)
            }
        }
        result.sort { lhs, rhs in
            if lhs.id == rhs.id { return lhs.devicePath < rhs.devicePath }
            return lhs.id < rhs.id
        }
        return result
    }

    // No suspension inside discovery/persistence transactions: an actor serializes
    // concurrent CLI clients and a failed write never changes cached state.
    func devices() throws -> [Device] {
        let live: [Device]
        do { live = try discover() } catch {
            throw RPCError(
                code: .unavailable,
                message: "USB serial inventory unavailable on target Agent"
            )
        }
        do { return Self.merge(live: live, saved: try saved()) } catch {
            throw RPCError(
                code: .internalError,
                message: "Cannot read robot arm bindings on target Agent"
            )
        }
    }

    func bind(_ request: Wendy_Agent_Services_V2_BindRobotArmRequest) throws -> Device {
        guard !request.id.isEmpty, request.model == "so101", request.name.utf8.count <= 128 else {
            throw RPCError(
                code: .invalidArgument,
                message: "Select a USB endpoint, model so101 and a name of at most 128 bytes"
            )
        }
        guard var d = try devices().first(where: { $0.id == request.id }) else {
            throw RPCError(
                code: .notFound,
                message: "USB endpoint is no longer present on target Agent; rescan"
            )
        }
        guard d.connected else {
            throw RPCError(
                code: .failedPrecondition,
                message: "USB identity is disconnected or ambiguous; rescan on target Agent"
            )
        }
        guard d.identityKind != "usb_location" || request.allowLocationBinding else {
            throw RPCError(
                code: .failedPrecondition,
                message:
                    "No USB serial ID; explicitly acknowledge a location binding that follows the USB port"
            )
        }
        d.configured = true
        d.model = "so101"
        d.verified = false
        d.name = request.name.trimmingCharacters(in: .whitespacesAndNewlines)
        if d.name.isEmpty { d.name = "SO-101" }
        do { try save(try saved().filter { $0.id != d.id } + [d]) } catch {
            throw RPCError(
                code: .internalError,
                message: "Cannot save robot arm binding on target Agent"
            )
        }
        return d
    }

    func forget(_ id: String) throws {
        guard !id.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "Robot arm binding ID is required")
        }
        do { try save(try saved().filter { $0.id != id }) } catch {
            throw RPCError(
                code: .internalError,
                message: "Cannot forget robot arm binding on target Agent"
            )
        }
    }

    func listRobotArms(
        request: ServerRequest<Wendy_Agent_Services_V2_ListRobotArmsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V2_ListRobotArmsResponse> {
        var response = Wendy_Agent_Services_V2_ListRobotArmsResponse()
        response.devices = try devices()
        return ServerResponse(message: response)
    }

    func bindRobotArm(
        request: ServerRequest<Wendy_Agent_Services_V2_BindRobotArmRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V2_BindRobotArmResponse> {
        var response = Wendy_Agent_Services_V2_BindRobotArmResponse()
        response.device = try bind(request.message)
        return ServerResponse(message: response)
    }

    func forgetRobotArm(
        request: ServerRequest<Wendy_Agent_Services_V2_ForgetRobotArmRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V2_ForgetRobotArmResponse> {
        try forget(request.message.id)
        return ServerResponse(message: Wendy_Agent_Services_V2_ForgetRobotArmResponse())
    }
}
