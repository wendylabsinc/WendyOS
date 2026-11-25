import Foundation
import Security

private final class MissionActivity: @unchecked Sendable {
    private let lock = NSLock()
    private var active = true
    private var revision: UInt64 = 1
    private var requests: [UUID: BoundedRequest] = [:]
    func setActive(_ value: Bool) {
        lock.lock()
        if active == value { lock.unlock(); return }
        active = value; revision &+= 1
        let cancel = Array(requests.values); requests.removeAll(); lock.unlock()
        cancel.forEach { $0.cancel() }
    }
    func begin() throws -> UInt64 {
        lock.lock(); defer { lock.unlock() }
        guard active else { throw CancellationError() }; return revision
    }
    func check(_ expected: UInt64) throws {
        lock.lock(); defer { lock.unlock() }
        guard active, revision == expected else { throw CancellationError() }
        try Task.checkCancellation()
    }
    func register(_ request: BoundedRequest, revision expected: UInt64) throws -> UUID {
        lock.lock(); defer { lock.unlock() }
        guard active, revision == expected else { throw CancellationError() }
        let id = UUID(); requests[id] = request; return id
    }
    func remove(_ id: UUID) { lock.lock(); requests.removeValue(forKey: id); lock.unlock() }
}

public actor MissionClient {
    public nonisolated let context: MissionContext
    public nonisolated let credentials: MissionCredentials
    private let pins: MissionClockPins
    private let monotonic: @Sendable () throws -> AnnotationClockReading
    private var lastReading: AnnotationClockReading?
    private var clockFault = false
    private nonisolated let activity = MissionActivity()
    public nonisolated func setMissionActive(_ active: Bool) { activity.setActive(active) }
    public func beginMissionOperation() throws -> UInt64 { try activity.begin() }

    public init(credentials: MissionCredentials, context: JSONValue, pins: MissionClockPins,
                navigationProfile: MissionNavigationProfile = .oneAxis,
                monotonic: @escaping @Sendable () throws -> AnnotationClockReading = AnnotationMonotonicClock.read) throws {
        self.credentials = credentials; self.pins = pins; self.monotonic = monotonic
        self.context = try MissionContext(json: context, credentials: credentials, pins: pins.value, navigationProfile: navigationProfile)
    }
    private func reading() throws -> AnnotationClockReading {
        guard !clockFault else { throw MissionError.clockUnavailable }
        let now = try monotonic()
        guard now.nanoseconds >= 0 else { clockFault = true; throw MissionError.clockUnavailable }
        if let previous = lastReading, now.bootID != previous.bootID || now.nanoseconds < previous.nanoseconds {
            clockFault = true; throw MissionError.clockUnavailable
        }
        lastReading = now; return now
    }
    private func freshClock(purpose: String, operation: UInt64? = nil) async throws -> MissionClockAnchor {
        // Only clock handshakes may retry. Mission delivery remains an explicit
        // action using the already persisted instruction bytes.
        let deadline = ContinuousClock.now + .seconds(2)
        for attempt in 0..<3 {
            try await Task.sleep(for: .milliseconds(attempt == 0 ? 60 : 100 * attempt))
            if let operation { try activity.check(operation) }
            let remaining = ContinuousClock.now.duration(to: deadline).components
            let seconds = Double(remaining.seconds) + Double(remaining.attoseconds) / 1e18
            guard seconds > 0 else { throw MissionError.clockUnavailable }
            do {
                let anchor = try await clockAttempt(purpose: purpose, operation: operation, timeout: min(2, seconds))
                guard ContinuousClock.now < deadline else { throw MissionError.clockUnavailable }
                return anchor
            } catch WorldClientError.httpStatus(429) where attempt < 2 {
                if let operation { try activity.check(operation) }
            }
        }
        throw MissionError.clockUnavailable
    }
    private func clockAttempt(purpose: String, operation: UInt64?, timeout: TimeInterval) async throws -> MissionClockAnchor {
        if let operation { try activity.check(operation) }
        let bytes = try MissionClockWire.request(credentials: credentials, pins: pins, purpose: purpose, nonce: Self.nonce())
        var request = URLRequest(url: credentials.endpoint.appendingPathComponent("v1/tasks/clock"))
        request.httpMethod = "POST"; request.httpBody = bytes; request.timeoutInterval = timeout
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
        let sent = try reading()
        let transport = BoundedRequest(limit: 64 << 10, resourceTimeout: timeout)
        let requestID = try operation.map { try activity.register(transport, revision: $0) }
        defer { if let requestID { activity.remove(requestID) } }
        let response = try await transport.run(request)
        if let operation { try activity.check(operation) }
        let received = try reading()
        guard received.bootID == sent.bootID, received.nanoseconds >= sent.nanoseconds else { throw MissionError.clockUnavailable }
        let observation = try JSONValue.decode(response, limit: 64 << 10)
        try MissionClockWire.verify(observation: observation, request: bytes, credentials: credentials, pins: pins, purpose: purpose)
        let anchor = MissionClockAnchor(observation: observation, received: received, rttNS: received.nanoseconds - sent.nanoseconds, purpose: purpose)
        _ = try clockInterval(anchor)
        return anchor
    }
    private func clockInterval(_ anchor: MissionClockAnchor) throws -> AnnotationTimeInterval {
        let interval = try anchor.interval(at: reading())
        guard interval.lowerUnixMS >= credentials.notBefore,
              anchor.purpose == "stop" || interval.upperUnixMS < credentials.notAfter else { throw MissionError.expired }
        return interval
    }
    public func catalog() async throws -> JSONValue {
        let result = try await request(path: "/v1/tasks/catalog", operation: "read")
        guard result["assignee"]?.string == context.assignee, result["domain"]?.string == "simulation", result["run_id"]?.string == context.runID,
              result["types"]?.array?.contains(where: { $0["type"]?.string == "wendy.nav.inspect.v1" && $0["class"]?.string == "MOVE" }) == true else { throw MissionError.unsupported }
        return result
    }
    public func proposals() async throws -> JSONValue { try await request(path: "/v1/proposals", operation: "read", query: [URLQueryItem(name: "limit", value: "100")]) }
    public func proposal(id: String) async throws -> MissionProposal {
        guard MissionWire.identifier(id) else { throw MissionError.invalidProposal }
        return try MissionProposal(view: await request(path: "/v1/proposals/" + id, operation: "read"), context: context, credentials: credentials)
    }
    public func prepareInstruction(draft: MissionDraft, operation: UInt64) async throws -> Data {
        try activity.check(operation)
        try context.validate(draft: draft)
        _ = try await catalog()
        try activity.check(operation)
        if let ref = draft.proposal {
            let current = try await proposal(id: ref.id)
            try activity.check(operation)
            guard current.reference == ref, current.target == draft.target,
                  current.triggerSHA256 == draft.trigger?.sha256,
                  current.inspectionDestination == draft.inspectionDestination else { throw MissionError.invalidProposal }
        }
        let anchor = try await freshClock(purpose: "mission", operation: operation)
        try activity.check(operation)
        let time = try clockInterval(anchor)
        if let until = draft.preparedUntilUnixMS { guard time.upperUnixMS < until else { throw MissionError.expired } }
        guard time.lowerUnixMS >= context.notBefore, time.upperUnixMS < context.notAfter,
              time.lowerUnixMS <= Int64.max - draft.durationMS else { throw MissionError.expired }
        let deadline = time.lowerUnixMS + draft.durationMS
        guard deadline > time.upperUnixMS, deadline <= context.notAfter, deadline <= credentials.notAfter else { throw MissionError.expired }
        let spec = try context.spec(draft: draft, deadline: deadline)
        try activity.check(operation)
        let finalTime = try clockInterval(anchor).upperUnixMS
        guard finalTime < deadline, draft.preparedUntilUnixMS.map({ finalTime < $0 }) ?? true else { throw MissionError.expired }
        return try credentials.signInstruction(operation: "mission", payload: spec, issuedAtUnixMS: time.lowerUnixMS, expiresAtUnixMS: deadline)
    }
    public func submit(instruction: Data, operation: UInt64) async throws -> JSONValue {
        try activity.check(operation)
        let value = try JSONValue.decode(instruction, limit: 128 << 10)
        guard value["audience"]?.string == credentials.audience, value["operation"]?.string == "mission", value["grant"] == credentials.grant,
              let expiry = value["expires_at_unix_ms"]?.int, value["payload"]?["control_context"] == context.signed else { throw MissionError.invalidJournal }
        return try await request(path: "/v1/tasks", operation: "mission", body: instruction, expiry: expiry, missionOperation: operation)
    }
    public func status(taskID: String) async throws -> JSONValue {
        guard MissionWire.identifier(taskID) else { throw MissionError.invalidDraft }
        return try await request(path: "/v1/tasks/status", operation: "read", query: [URLQueryItem(name: "task_id", value: taskID)])
    }
    public func prepareStop(taskID: String) async throws -> Data {
        guard MissionWire.identifier(taskID) else { throw MissionError.invalidDraft }
        let anchor = try await freshClock(purpose: "stop")
        let time = try clockInterval(anchor)
        let payload = GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(taskID))])
        guard try clockInterval(anchor).upperUnixMS < time.lowerUnixMS + 5_000 else { throw MissionError.expired }
        return try credentials.signInstruction(operation: "stop", payload: payload, issuedAtUnixMS: time.lowerUnixMS, expiresAtUnixMS: time.lowerUnixMS + 5_000)
    }
    public func submitStop(instruction: Data) async throws -> JSONValue {
        try await submitStopRecord(instruction: instruction).value
    }
    public func submitStopRecord(instruction: Data) async throws -> (value: JSONValue, originalJSON: Data?) {
        let value = try JSONValue.decode(instruction)
        guard value["operation"]?.string == "stop", value["grant"] == credentials.grant, let expiry = value["expires_at_unix_ms"]?.int else { throw MissionError.invalidJournal }
        let raw = try await requestBytes(path: "/v1/tasks/stop", operation: "stop", body: instruction, expiry: expiry)
        return (try JSONValue.decode(raw, limit: 128 << 10), raw)
    }
    private func request(path: String, operation: String, body: Data = Data(), query: [URLQueryItem] = [], expiry: Int64? = nil, missionOperation: UInt64? = nil) async throws -> JSONValue {
        try JSONValue.decode(await requestBytes(path: path, operation: operation, body: body, query: query, expiry: expiry, missionOperation: missionOperation), limit: 128 << 10)
    }
    private func requestBytes(path: String, operation: String, body: Data = Data(), query: [URLQueryItem] = [], expiry: Int64? = nil, missionOperation: UInt64? = nil) async throws -> Data {
        let session = operation == "stop" ? nil : try missionOperation ?? activity.begin()
        let anchor = try await freshClock(purpose: operation == "stop" ? "stop" : "mission", operation: session)
        if let session { try activity.check(session) }
        if let expiry, try clockInterval(anchor).upperUnixMS >= expiry { throw MissionError.expired }
        var parts = URLComponents(url: credentials.endpoint, resolvingAgainstBaseURL: false)!
        parts.path = path; parts.queryItems = query.isEmpty ? nil : query
        guard let url = parts.url else { throw WorldClientError.invalidEndpoint }
        var request = URLRequest(url: url); request.timeoutInterval = operation == "stop" ? 3 : 10
        request.httpMethod = body.isEmpty ? "GET" : "POST"; request.httpBody = body.isEmpty ? nil : body
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
        let time = try clockInterval(anchor)
        if let expiry, time.upperUnixMS >= expiry { throw MissionError.expired }
        request.setValue(try credentials.signedAuthorization(operation: operation, body: body, nonce: Self.nonce(), issuedAtUnixMS: time.lowerUnixMS).base64EncodedString(), forHTTPHeaderField: "X-World-Authorization")
        let transport = BoundedRequest(limit: 128 << 10, resourceTimeout: operation == "stop" ? 3 : 10)
        let requestID = try session.map { try activity.register(transport, revision: $0) }
        defer { if let requestID { activity.remove(requestID) } }
        if let session { try activity.check(session) }
        let dispatchTime = try clockInterval(anchor)
        guard dispatchTime.upperUnixMS < time.lowerUnixMS + 5_000 else { throw MissionError.expired }
        if let expiry, dispatchTime.upperUnixMS >= expiry { throw MissionError.expired }
        let data = try await transport.run(request)
        if let session { try activity.check(session) }
        return data
    }
    static func nonce() throws -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else { throw MissionError.clockUnavailable }
        return bytes.map { String(format: "%02x", $0) }.joined()
    }
}
