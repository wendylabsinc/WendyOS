import Foundation
import Security

/// Reads one explicitly configured simulation gateway. HTTP responses have
/// passed that node's validation; this API does not verify each origin record.
public actor WorldClient {
    let credentials: WorldCredentials
    private var clockAnchor: AnnotationClockAnchor?
    private var clockLastNS: Int64?
    private var clockFaulted = false
    private let monotonic: @Sendable () throws -> AnnotationClockReading
    private var explicitRequestTime: (@Sendable () throws -> Int64)?
    public init(credentials: WorldCredentials) { self.credentials = credentials; self.monotonic = AnnotationMonotonicClock.read }
    init(credentials: WorldCredentials, requestTime: @escaping @Sendable () throws -> Int64) {
        self.credentials = credentials; self.monotonic = AnnotationMonotonicClock.read; self.explicitRequestTime = requestTime
    }
    public func restoreClock(_ anchor: AnnotationClockAnchor) throws {
        let reading = try monotonic()
        // Restoring a cached anchor never clears a fault observed in this process.
        guard !clockFaulted, reading.nanoseconds >= (clockLastNS ?? 0) else { throw AnnotationClockError.unavailable }
        _ = try anchor.at(reading, credentials: credentials); clockAnchor = anchor; clockLastNS = reading.nanoseconds
    }
    public func currentClock() throws -> AnnotationClockEvidence {
        guard let clockAnchor, !clockFaulted else { throw AnnotationClockError.unavailable }
        let reading = try monotonic()
        guard reading.bootID == clockAnchor.bootID, reading.nanoseconds >= (clockLastNS ?? clockAnchor.receivedNS) else {
            clockFaulted = true; throw AnnotationClockError.unavailable
        }
        let evidence = try clockAnchor.at(reading, credentials: credentials)
        clockLastNS = reading.nanoseconds
        return evidence
    }
    @discardableResult public func bootstrapAnnotationClock() async throws -> AnnotationClockAnchor {
        guard credentials.canAnnotate, let pins = credentials.clockPins,
              let url = URL(string: "/v1/annotations/clock", relativeTo: credentials.endpoint)?.absoluteURL else { throw AnnotationClockError.unavailable }
        var nonceBytes = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, nonceBytes.count, &nonceBytes) == errSecSuccess else { throw AnnotationClockError.unavailable }
        let nonce = nonceBytes.map { String(format: "%02x", $0) }.joined()
        let message = JSONValue.object(["tenant": .string(credentials.tenant), "site": .string(credentials.site), "audience": .string(credentials.audience), "run_id": .string(pins.runID), "clock_domain": .string("simulation:" + pins.runID), "nonce": .string(nonce)])
        let bytes = try ClockWire.message(message, keys: ClockWire.requestKeys)
        let signature = try credentials.key.signature(for: ClockWire.signingBytes(grant: credentials.grant, message: bytes, name: "request"))
        let proof = JSONValue.object(["version": .integer(1), "grant": credentials.grant, "request": message, "signature": .string(signature.base64EncodedString())])
        let body = try ClockWire.signed(proof, message: "request", keys: ClockWire.requestKeys)
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 2)
        request.httpMethod = "POST"; request.httpBody = body
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let sent = try monotonic()
        let response = try await BoundedRequest(limit: 64 << 10).run(request)
        let received = try monotonic()
        guard sent.bootID == received.bootID, received.nanoseconds >= sent.nanoseconds else { throw AnnotationClockError.unavailable }
        let evidence = AnnotationClockEvidence(request: proof, observation: try JSONValue.decode(response, limit: 64 << 10), rttNS: received.nanoseconds - sent.nanoseconds, elapsedNS: 0)
        try evidence.verify(credentials: credentials)
        let anchor = AnnotationClockAnchor(evidence: evidence, bootID: received.bootID, receivedNS: received.nanoseconds)
        let checked = try monotonic()
        _ = try anchor.at(checked, credentials: credentials)
        clockAnchor = anchor; clockLastNS = checked.nanoseconds; clockFaulted = false
        return anchor
    }
    private func requestTime() async throws -> Int64 {
        if let explicitRequestTime { return try explicitRequestTime() }
        if credentials.canAnnotate {
            if (try? currentClock()) == nil { try await bootstrapAnnotationClock() }
            let interval = try currentClock().interval()
            guard interval.lowerUnixMS >= credentials.notBefore, interval.upperUnixMS < credentials.notAfter else { throw WorldClientError.expiredCredentials }
            return interval.lowerUnixMS
        }
        return Int64(Date().timeIntervalSince1970 * 1000)
    }

    public func query() async throws -> WorldSnapshot {
        let snapshot = try WorldSnapshot(data: await request(path: "/v1/components"))
        guard snapshot.components.allSatisfy({
            $0.raw["key"]?["tenant"]?.string == credentials.tenant && $0.raw["key"]?["site"]?.string == credentials.site
        }) else { throw WorldClientError.invalidResponse }
        return snapshot
    }

    public func sites() async throws -> JSONValue {
        let value = try JSONValue.decode(await request(path: "/v1/sites"))
        guard let publications = value["publications"]?.array,
              publications.allSatisfy({
                  let component = $0["component"]
                  return component?["key"]?["tenant"]?.string == credentials.tenant &&
                      component?["key"]?["site"]?.string == credentials.site &&
                      component?["payload"]?["site"]?.string == credentials.site
              }) else { throw WorldClientError.invalidResponse }
        return value
    }

    public func taskRelay() async throws -> JSONValue {
        let value = try JSONValue.decode(await request(path: "/v1/task-relay"))
        guard value["entries"]?.array != nil, value["last_known"]?.bool == true else { throw WorldClientError.invalidResponse }
        return value
    }

    public func annotationEpoch() async throws -> UInt64 {
        guard credentials.canAnnotate else { throw AnnotationError.notAuthorized }
        let value = try JSONValue.decode(await request(path: "/v1/annotations/epoch", operation: "annotation-write", body: Data("{}".utf8), limit: 4096), limit: 4096)
        guard let epoch = value["epoch"]?.uint, epoch > 0 else { throw WorldClientError.invalidResponse }
        return epoch
    }

    public func annotationScopeID(context: AnnotationContext) throws -> String {
        try credentials.annotationScopeID(context: context)
    }

    public func publishAnnotation(_ body: Data, draft: AnnotationDraft) async throws {
        try credentials.validateAnnotation(body, draft: draft)
        let response = try JSONValue.decode(await request(path: "/v1/annotations", operation: "annotation-write", body: body, limit: 128 << 10), limit: 128 << 10)
        guard ["applied", "duplicate"].contains(response["result"]?.string ?? ""),
              response["component"] == (try JSONValue.decode(body))["component"] else { throw AnnotationError.invalidRecord }
    }

    public func publishAnnotationAmendment(_ body: Data, original: AnnotationEntry) async throws {
        _ = try credentials.decodeAmendment(body, original: original)
        let response = try JSONValue.decode(await request(path: "/v1/annotations", operation: "annotation-write", body: body, limit: 128 << 10), limit: 128 << 10)
        guard ["applied", "duplicate"].contains(response["result"]?.string ?? ""), response["component"] == (try JSONValue.decode(body))["component"] else { throw AnnotationError.invalidRecord }
    }
    public func annotationViews() async throws -> [JSONValue] {
        var rows: [JSONValue] = [], revision = "", offset: Int64 = 0
        for _ in 0..<512 {
            let body = GoAuthorityJSON.object([("revision", GoAuthorityJSON.string(revision)), ("offset", Data(String(offset).utf8)), ("limit", Data("8".utf8))])
            let started = try AnnotationMonotonicClock.read()
            let page = try JSONValue.decode(await request(path: "/v1/annotations/views", operation: "read", body: body))
            let received = try AnnotationMonotonicClock.read()
            guard started.bootID == received.bootID, received.nanoseconds >= started.nanoseconds, let observed = page["observed_at"]?.string.flatMap(WorldComponent.parseTime) else { throw WorldClientError.invalidResponse }
            guard let next = page["next_offset"]?.int, let entries = page["views"]?.array, entries.count <= 8,
                  next == offset + Int64(entries.count), let rev = page["revision"]?.string, Self.isDigest(rev),
                  revision.isEmpty || revision == rev, let complete = page["complete"]?.bool,
                  complete || !entries.isEmpty else { throw WorldClientError.invalidResponse }
            for entry in entries {
                guard entry["original_key"]?["tenant"]?.string == credentials.tenant, entry["original_key"]?["site"]?.string == credentials.site,
                      let id = entry["original_sha256"]?.string, Self.isDigest(id), let expiry = entry["expires_at_unix_ms"]?.int, expiry > 0,
                      let amendments = entry["amendment_sha256"]?.array, amendments.count <= 32 else { throw WorldClientError.invalidResponse }
            }
            for entry in entries {
                guard var fields = entry.object, let until = entry["verification_until_unix_ms"]?.int, until > 0, until < 253_402_214_400_000 else { throw WorldClientError.invalidResponse }
                let remaining = max(Int64(0), until - Int64(observed.timeIntervalSince1970 * 1000))
                guard remaining <= 86_400_000 else { throw WorldClientError.invalidResponse }
                fields["display_boot_id"] = .string(received.bootID)
                fields["display_until_ns"] = .integer(received.nanoseconds + max(0, remaining * 1_000_000 - (received.nanoseconds - started.nanoseconds)))
                rows.append(.object(fields))
            }
            if complete { return rows }; offset = next; revision = rev
        }
        throw WorldClientError.responseTooLarge
    }

    /// Downloads from this credential's fixed gateway, never from a URL carried
    /// in an observation. Failed or truncated transfers return no evidence.
    public func downloadEvidence(digest: String, maxBytes: Int = 8 << 20) async throws -> VerifiedEvidence {
        guard Self.isDigest(digest), maxBytes > 0, maxBytes <= 64 << 20,
              credentials.scopes.contains("evidence-read") else { throw WorldClientError.invalidEvidence }
        var bytes = Data()
        var total: Int64?
        var contentType: String?
        var retention: Int64?
        repeat {
            try Task.checkCancellation()
            let body = GoAuthorityJSON.object([
                ("sha256", GoAuthorityJSON.string(digest)),
                ("offset", Data(String(bytes.count).utf8)), ("length", Data(String(48 << 10).utf8))
            ])
            let value = try JSONValue.decode(await request(path: "/v1/evidence/get", operation: "evidence-read", body: body, limit: 128 << 10), limit: 128 << 10)
            guard value["sha256"]?.string == digest,
                  let size = value["size"]?.int, size > 0, size <= Int64(maxBytes),
                  value["offset"]?.int == Int64(bytes.count),
                  let encoded = value["data"]?.string, let chunk = Data(base64Encoded: encoded),
                  !chunk.isEmpty, chunk.count <= 48 << 10, Int64(chunk.count) <= size - Int64(bytes.count),
                  let type = value["content_type"]?.string, !type.isEmpty, type.utf8.count <= 256,
                  let until = value["retain_until_unix_ms"]?.int, Double(until) / 1000 > Date().timeIntervalSince1970,
                  total == nil || total == size, contentType == nil || contentType == type,
                  retention == nil || retention == until else { throw WorldClientError.invalidEvidence }
            total = size; contentType = type; retention = until
            bytes.append(chunk)
        } while Int64(bytes.count) < total!
        guard WorldCredentials.digest(bytes) == digest, let contentType, let retention else { throw WorldClientError.invalidEvidence }
        return VerifiedEvidence(data: bytes, sha256: digest, contentType: contentType, retainUntil: Date(timeIntervalSince1970: Double(retention) / 1000))
    }

    static func isDigest(_ value: String) -> Bool {
        value.utf8.count == 64 && value.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
    }

    /// Reads exact capture-owner ranges through this profile's native evidence
    /// bridge. Result expiry does not shorten or renew the media retention.
    public func downloadNativeEvidence(_ reference: NativeEvidenceReference) async throws -> VerifiedEvidence {
        guard credentials.scopes.contains("evidence-read") else { throw WorldClientError.missingReadScope }
        var bytes = Data(), retention: Int64?
        while bytes.count < reference.size {
            try Task.checkCancellation()
            let count = min(48 << 10, reference.size - bytes.count)
            var fields = reference.raw.object!
            fields["offset"] = .integer(Int64(bytes.count)); fields["length"] = .integer(Int64(count))
            let response = try JSONValue.decode(await request(path: "/v1/native-evidence/get", operation: "evidence-read",
                body: JSONValue.object(fields).encoded(), limit: 128 << 10), limit: 128 << 10)
            guard response["source"] == reference.source, response["sha256"]?.string == reference.sha256,
                  response["size"]?.int == Int64(reference.size), response["content_type"]?.string == reference.contentType,
                  response["offset"]?.int == Int64(bytes.count), let encoded = response["data"]?.string,
                  let chunk = Data(base64Encoded: encoded), chunk.count == count,
                  let until = response["retain_until_unix_ms"]?.int,
                  retention == nil || retention == until else { throw WorldClientError.invalidEvidence }
            guard Double(until) / 1000 > Date().timeIntervalSince1970 else { throw InspectionMediaCacheError.expired }
            retention = until; bytes.append(chunk)
        }
        guard WorldCredentials.digest(bytes) == reference.sha256, let until = retention else { throw WorldClientError.invalidEvidence }
        guard Double(until) / 1000 > Date().timeIntervalSince1970 else { throw InspectionMediaCacheError.expired }
        return VerifiedEvidence(data: bytes, sha256: reference.sha256, contentType: reference.contentType,
                                retainUntil: Date(timeIntervalSince1970: Double(until) / 1000))
    }

    func request(path: String, operation: String = "read", body: Data = Data(), limit: Int = 16 << 20) async throws -> Data {
        guard ["/v1/query", "/v1/watch/selected", "/v1/components", "/v1/sites", "/v1/task-relay", "/v1/evidence/get", "/v1/native-evidence/get", "/v1/annotations/epoch", "/v1/annotations", "/v1/annotations/views"].contains(path),
              let url = URL(string: path, relativeTo: credentials.endpoint)?.absoluteURL else { throw WorldClientError.invalidEndpoint }
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 15)
        request.httpMethod = body.isEmpty ? "GET" : "POST"
        if !body.isEmpty { request.httpBody = body; request.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let issued = try await requestTime()
        request.setValue(try credentials.signedAuthorization(operation: operation, body: body, nonce: UUID().uuidString, issuedAtUnixMS: issued).base64EncodedString(), forHTTPHeaderField: "X-World-Authorization")
        return try await BoundedRequest(limit: limit).run(request)
    }
}
