import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SwiftProtobuf

struct NotificationForm: Sendable {
    var userIDs = ""
    var roles: [Int] = []
    var title = ""
    var body = ""
    var severity = "1"
    var deepLink = "wendy://devices/current/live"
    var notificationID = UUID().uuidString.lowercased()
    var metadata = ""

    init() {}

    init(encoded: String) {
        self.init()
        var fields: [String: [String]] = [:]
        for pair in encoded.split(separator: "&") {
            let parts = pair.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
            let decode: (Substring) -> String = {
                let text = $0.replacingOccurrences(of: "+", with: " ")
                return text.removingPercentEncoding ?? text
            }
            fields[decode(parts[0]), default: []].append(parts.count == 2 ? decode(parts[1]) : "")
        }
        userIDs = fields["userIDs"]?.first ?? ""
        roles = (fields["roles"] ?? []).compactMap(Int.init)
        title = fields["title"]?.first ?? ""
        body = fields["body"]?.first ?? ""
        severity = fields["severity"]?.first ?? "0"
        deepLink = fields["deepLink"]?.first ?? ""
        notificationID = fields["notificationID"]?.first ?? ""
        metadata = fields["metadata"]?.first ?? ""
    }

    func message() throws -> Wendy_System_V1_SendRequest {
        var request = Wendy_System_V1_SendRequest()
        request.audience.userIds = userIDs.split(whereSeparator: \.isNewline).map(String.init)
        request.audience.roles = roles.map { .init(rawValue: $0) ?? .UNRECOGNIZED($0) }
        request.title = title
        request.body = body
        request.severity = .init(rawValue: Int(severity) ?? 0) ?? .unspecified
        request.deepLink = deepLink
        request.notificationID = notificationID
        if !metadata.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            request.metadata = try Google_Protobuf_Struct(jsonString: metadata)
        }
        return request
    }
}

struct SendOutcome: Sendable {
    let notificationID: String
    let code: String
    let message: String
    let error: (any Error)?

    var created: Bool { error == nil }
    var underlyingCause: String? {
        (error as? RPCError)?.cause.map { String(describing: $0) }
    }

    static func failure(_ error: any Error, notificationID: String, context: String) -> Self {
        if let rpc = error as? RPCError {
            return .init(notificationID: notificationID, code: "\(rpc.code)", message: rpc.message, error: error)
        }
        return .init(notificationID: notificationID, code: context, message: String(describing: error), error: error)
    }
}

struct NotificationSender: Sendable {
    let socketPath: String?

    init(socketPath: String? = ProcessInfo.processInfo.environment["WENDY_SYSTEM_SOCKET"]) {
        self.socketPath = socketPath
    }

    func send(_ form: NotificationForm) async -> SendOutcome {
        let request: Wendy_System_V1_SendRequest
        do {
            request = try form.message()
        } catch {
            return .failure(error, notificationID: form.notificationID, context: "Metadata JSON")
        }
        guard let socketPath, !socketPath.isEmpty else {
            return .failure(
                SocketError(message: "WENDY_SYSTEM_SOCKET is missing. Run on WendyOS with the notifications entitlement in wendy.json."),
                notificationID: form.notificationID, context: "System socket"
            )
        }
        guard FileManager.default.fileExists(atPath: socketPath) else {
            return .failure(
                SocketError(message: "System socket does not exist at \(socketPath). Check the notifications entitlement in wendy.json and the Agent version."),
                notificationID: form.notificationID, context: "System socket"
            )
        }
        do {
            let transport = try HTTP2ClientTransport.Posix(
                target: .unixDomainSocket(path: socketPath), transportSecurity: .plaintext
            )
            let id = try await withGRPCClient(transport: transport) { client in
                let notifications = Wendy_System_V1_NotificationService.Client(wrapping: client)
                var options = CallOptions.defaults
                options.timeout = .seconds(20)
                return try await notifications.send(request, options: options) { response in
                    try response.message.notificationID
                }
            }
            return .init(notificationID: id, code: "Created", message: "Created notification \(id)", error: nil)
        } catch {
            return .failure(error, notificationID: form.notificationID, context: "Connection")
        }
    }
}

private struct SocketError: Error, CustomStringConvertible {
    let message: String
    var description: String { message }
}
