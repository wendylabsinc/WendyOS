import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
import Hummingbird
import HummingbirdTesting
import Logging
import Testing
@testable import WendyNotify

private final class RequestLogs: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [String] = []
    func append(_ line: String) {
        lock.lock()
        defer { lock.unlock() }
        lines.append(line)
    }
    func snapshot() -> String {
        lock.lock()
        defer { lock.unlock() }
        return lines.joined(separator: "\n")
    }
}

private struct RequestLogHandler: LogHandler {
    let captured: RequestLogs
    var metadata: Logger.Metadata = [:]
    var logLevel: Logger.Level = .trace
    subscript(metadataKey key: String) -> Logger.Metadata.Value? {
        get { metadata[key] }
        set { metadata[key] = newValue }
    }
    func log(level: Logger.Level, message: Logger.Message, metadata: Logger.Metadata?,
             source: String, file: String, function: String, line: UInt) {
        captured.append("\(level) \(message) \(self.metadata) \(metadata ?? [:])")
    }
}

private actor InertNotificationSink {
    struct Call: Sendable {
        let message: Wendy_System_V1_SendRequest
        let metadata: Metadata
    }
    private var calls: [Call] = []
    let trailerToken: String
    init(trailerToken: String) { self.trailerToken = trailerToken }
    func receive(_ request: ServerRequest<Wendy_System_V1_SendRequest>) throws -> Wendy_System_V1_SendResponse {
        calls.append(.init(message: request.message, metadata: request.metadata))
        if request.message.title == "refuse this fixture" {
            var metadata = Metadata()
            metadata.addString("Bearer \(trailerToken)", forKey: "authorization")
            throw RPCError(code: .permissionDenied, message: "Fixture permission refused", metadata: metadata)
        }
        var response = Wendy_System_V1_SendResponse()
        response.notificationID = request.message.notificationID
        return response
    }
    func snapshot() -> [Call] { calls }
}

private struct InertNotificationService: RegistrableRPCService {
    let sink: InertNotificationSink
    func registerMethods<Transport: ServerTransport>(with router: inout RPCRouter<Transport>) {
        router.registerHandler(
            forMethod: Wendy_System_V1_NotificationService.Method.Send.descriptor,
            deserializer: ProtobufDeserializer<Wendy_System_V1_SendRequest>(),
            serializer: ProtobufSerializer<Wendy_System_V1_SendResponse>()
        ) { request, _ in
            let unary = try await ServerRequest(stream: request)
            let response = try await sink.receive(unary)
            return StreamingServerResponse(single: ServerResponse(message: response))
        }
    }
}

@Test(.timeLimit(.minutes(1)))
func `live HTTP sends use only the private socket and do not disclose ignored credentials`() async throws {
    // All credential markers are generated per test and have no authority.
    let directory = URL(fileURLWithPath: "/tmp/wn-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                          attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: directory) }
    let socketPath = directory.appendingPathComponent("agent.sock").path
    let authorization = UUID().uuidString
    let legacyKey = UUID().uuidString
    let queryToken = UUID().uuidString
    let trailerToken = UUID().uuidString
    let secrets = [authorization, legacyKey, queryToken, trailerToken]
    let sink = InertNotificationSink(trailerToken: trailerToken)
    let logs = RequestLogs()
    let logger = Logger(label: "WendyNotify.request-regression") { _ in RequestLogHandler(captured: logs) }
    let firstID = UUID().uuidString.lowercased()
    let secondID = UUID().uuidString.lowercased()
    let transport = HTTP2ServerTransport.Posix(address: .unixDomainSocket(path: socketPath), transportSecurity: .plaintext)
    let server = GRPCServer(transport: transport, services: [InertNotificationService(sink: sink)])
    try await withThrowingDiscardingTaskGroup { group in
        group.addTask { try await server.serve() }
        defer { server.beginGracefulShutdown() }
        _ = try await server.listeningAddress
        let app = Application(router: makeRouter(sender: .init(socketPath: socketPath)), logger: logger)
        // Real loopback HTTP and the production NotificationSender's UDS gRPC
        // path run here. The private service records requests and sends no push.
        try await app.test(.live) { client in
            try await client.execute(uri: "/", method: .get) { response in
                #expect(response.status == .ok)
                #expect(String(buffer: response.body).contains("Compose notification"))
                #expect(!String(buffer: response.body).contains("name=\"accessKey\""))
            }
            try await client.execute(uri: "/", method: .post,
                                     body: .init(string: "notificationID=\(firstID)&title=Hello+fixture&body=Private+socket+only&roles=1&roles=3&severity=2&deepLink=wendy%3A%2F%2Fdevices%2Fcurrent%2Flive")) { response in
                #expect(response.status == .ok)
                #expect(String(buffer: response.body).contains("Created notification \(firstID)"))
            }
            try await client.execute(uri: "/?key=\(legacyKey)&token=\(queryToken)", method: .get,
                                     headers: [.authorization: "Bearer \(authorization)"]) { response in
                #expect(response.status == .ok)
                let html = String(buffer: response.body)
                #expect(html.contains(firstID))
                for secret in secrets { #expect(!html.contains(secret)) }
                #expect(response.headers[.cacheControl] == "no-store")
                #expect(response.headers[.init("Referrer-Policy")!] == "no-referrer")
            }
            try await client.execute(uri: "/?key=\(legacyKey)&token=\(queryToken)", method: .post,
                                     headers: [.authorization: "Bearer \(authorization)"],
                                     body: .init(string: "notificationID=\(secondID)&title=refuse+this+fixture&body=refusal&accessKey=\(legacyKey)&token=\(queryToken)")) { response in
                #expect(response.status == .ok)
                let html = String(buffer: response.body)
                #expect(html.contains("Fixture permission refused"))
                #expect(html.contains(secondID))
                for secret in secrets { #expect(!html.contains(secret)) }
            }
            try await client.execute(uri: "/", method: .get) { response in
                let html = String(buffer: response.body)
                #expect(html.contains(firstID))
                #expect(html.contains(secondID))
                for secret in secrets { #expect(!html.contains(secret)) }
                #expect(response.headers[.init("X-Frame-Options")!] == "DENY")
                #expect(response.headers[.init("X-Content-Type-Options")!] == "nosniff")
            }
        }
    }
    let calls = await sink.snapshot()
    #expect(calls.count == 2)
    let first = try #require(calls.first)
    #expect(first.message.notificationID == firstID)
    #expect(first.message.title == "Hello fixture")
    #expect(first.message.body == "Private socket only")
    #expect(first.message.audience.roles == [.owner, .billingManager])
    #expect(first.message.severity.rawValue == 2)
    #expect(first.message.deepLink == "wendy://devices/current/live")
    for call in calls {
        #expect(Array(call.metadata["authorization"]).isEmpty)
        for secret in secrets { #expect(!String(describing: call.message).contains(secret)) }
    }
    let captured = logs.snapshot()
    #expect(captured.contains("Notification send failed"))
    #expect(captured.contains("Fixture permission refused"))
    for secret in secrets { #expect(!captured.contains(secret)) }
}
