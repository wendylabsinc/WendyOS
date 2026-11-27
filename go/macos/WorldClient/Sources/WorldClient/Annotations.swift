import CryptoKit
import Foundation

public enum AnnotationError: Error, LocalizedError, Sendable {
    case invalidNote, notAuthorized, invalidRecord, invalidOutbox, changedOutbox, outboxFull
    public var errorDescription: String? {
        switch self {
        case .invalidNote: "Use nonblank text up to 2,048 UTF-8 bytes and three bounded millimetre coordinates. Notes expire within 24 hours."
        case .notAuthorized: "This profile cannot author simulation annotations."
        case .invalidRecord: "The signed annotation or acknowledgement does not match the saved note."
        case .invalidOutbox: "The annotation outbox failed validation. No notes were sent."
        case .changedOutbox: "Another process changed this outbox. Reopen the profile before continuing."
        case .outboxFull: "The bounded annotation outbox is full."
        }
    }
}

public struct AnnotationContext: Codable, Sendable, Equatable {
    public let runID: String
    public let frameID: String
    public let frameRevision: String
    public init(runID: String, frameID: String, frameRevision: String) throws {
        guard [runID, frameID, frameRevision].allSatisfy({ !$0.isEmpty && $0.utf8.count <= 256 }) else { throw AnnotationError.invalidNote }
        self.runID = runID; self.frameID = frameID; self.frameRevision = frameRevision
    }
    public init(json: JSONValue) throws {
        guard let fields = json.object, Set(fields.keys) == ["run_id", "frame"],
              let frame = json["frame"]?.object, Set(frame.keys) == ["id", "revision"],
              let run = json["run_id"]?.string, let id = frame["id"]?.string,
              let revision = frame["revision"]?.string else { throw AnnotationError.invalidNote }
        try self.init(runID: run, frameID: id, frameRevision: revision)
    }
}

public struct AnnotationDraft: Codable, Sendable, Equatable, Identifiable {
    public let id: String
    public let authorPrincipal: String
    public let text: String
    public let positionMM: [Int64]
    public let authoredAtUnixMS: Int64
    public let expiresAtUnixMS: Int64
    public let context: AnnotationContext
    public let schemaVersion: Int?
    public let clock: AnnotationClockEvidence?
    public let requestedLifetimeMS: Int64?

    public init(id: String = UUID().uuidString.lowercased(), authorPrincipal: String, text: String, positionMM: [Int64], authoredAtUnixMS: Int64, expiresAtUnixMS: Int64, context: AnnotationContext) throws {
        self.id = id; self.authorPrincipal = authorPrincipal; self.text = text; self.positionMM = positionMM
        self.authoredAtUnixMS = authoredAtUnixMS; self.expiresAtUnixMS = expiresAtUnixMS; self.context = context
        self.schemaVersion = nil; self.clock = nil; self.requestedLifetimeMS = nil
        try validate()
    }
    init(id: String = UUID().uuidString.lowercased(), authorPrincipal: String, text: String, positionMM: [Int64], lifetimeMS: Int64, context: AnnotationContext, clock: AnnotationClockEvidence?) throws {
        let interval = try clock?.interval()
        self.id = id; self.authorPrincipal = authorPrincipal; self.text = text; self.positionMM = positionMM
        authoredAtUnixMS = interval?.lowerUnixMS ?? 0
        expiresAtUnixMS = interval.map { $0.lowerUnixMS + lifetimeMS } ?? 0
        self.context = context; schemaVersion = 2; self.clock = clock; requestedLifetimeMS = lifetimeMS
        try validate()
    }
    func validate() throws {
        guard id.range(of: "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", options: .regularExpression) != nil,
              !authorPrincipal.isEmpty, authorPrincipal.utf8.count <= 256,
              !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, text.utf8.count <= 2048,
              positionMM.count == 3, positionMM.allSatisfy({ (-1_000_000_000...1_000_000_000).contains($0) }),
              schemaVersion == nil || schemaVersion == 2 else { throw AnnotationError.invalidNote }
        if schemaVersion == 2 {
            guard let lifetime = requestedLifetimeMS, lifetime > 0, lifetime <= 86_400_000 else { throw AnnotationError.invalidNote }
            if let clock {
                let interval = try clock.interval()
                guard authoredAtUnixMS == interval.lowerUnixMS, expiresAtUnixMS == authoredAtUnixMS + lifetime else { throw AnnotationError.invalidNote }
            } else {
                guard authoredAtUnixMS == 0, expiresAtUnixMS == 0 else { throw AnnotationError.invalidNote }
            }
        }
        if schemaVersion != 2 || clock != nil {
            guard authoredAtUnixMS > 0, authoredAtUnixMS < 253_402_214_400_000,
                  expiresAtUnixMS > authoredAtUnixMS, expiresAtUnixMS - authoredAtUnixMS <= 86_400_000 else { throw AnnotationError.invalidNote }
        }
        _ = try AnnotationContext(runID: context.runID, frameID: context.frameID, frameRevision: context.frameRevision)
    }
}

public enum AnnotationState: String, Codable, Sendable {
    case untimed, draft, pending, published, conflict, expired, denied, rejected
}

public struct AnnotationEntry: Codable, Sendable, Equatable, Identifiable {
    public let draft: AnnotationDraft
    public internal(set) var state: AnnotationState
    public internal(set) var signedRecord: Data?
    public internal(set) var issue: String?
    public internal(set) var acknowledgedAtUnixMS: Int64?
    public var id: String { draft.id }
    public var bodySHA256: String? { signedRecord.map(WorldCredentials.digest) }
    public var epoch: UInt64? { signedRecord.flatMap { try? JSONValue.decode($0)["component"]?["version"]?["epoch"]?.uint } }
    public var sequence: UInt64? { signedRecord.flatMap { try? JSONValue.decode($0)["component"]?["version"]?["sequence"]?.uint } }
}

extension WorldCredentials {
    public var canAnnotate: Bool { scopes.contains("annotation-write") }

    public func annotationScopeID(context: AnnotationContext) throws -> String {
        guard canAnnotate else { throw AnnotationError.notAuthorized }
        let source = try GoAuthorityJSON.source(grant["grant"]?["source"])
        var fields: [(String, Data)] = [
            ("reader_scope", GoAuthorityJSON.string(scopeID)), ("principal", GoAuthorityJSON.string(principal)),
            ("source", source), ("run_id", GoAuthorityJSON.string(context.runID)),
            ("frame_id", GoAuthorityJSON.string(context.frameID)), ("frame_revision", GoAuthorityJSON.string(context.frameRevision))
        ]
        if let pins = clockPins {
            fields.append(("clock_trust", GoAuthorityJSON.object([
                ("root_public_key", GoAuthorityJSON.string(pins.rootPublicKey.base64EncodedString())),
                ("gateway_principal", GoAuthorityJSON.string(pins.gatewayPrincipal)),
                ("gateway_key_sha256", GoAuthorityJSON.string(pins.gatewayKeySHA256)),
                ("run_id", GoAuthorityJSON.string(pins.runID)), ("clock_domain", GoAuthorityJSON.string(pins.clockDomain))
            ])))
        }
        return Self.digest(GoAuthorityJSON.object(fields))
    }

    /// Narrow signer: the component schema, provenance and authority are fixed.
    public func signAnnotation(_ draft: AnnotationDraft, epoch: UInt64, sequence: UInt64) throws -> Data {
        guard canAnnotate, draft.authorPrincipal == principal,
              draft.authoredAtUnixMS >= notBefore, draft.authoredAtUnixMS < notAfter else { throw AnnotationError.notAuthorized }
        if draft.schemaVersion == 2 {
            guard let clock = draft.clock else { throw AnnotationClockError.unavailable }
            try clock.verify(credentials: self)
            let interval = try clock.interval()
            guard interval.lowerUnixMS >= notBefore, interval.upperUnixMS < notAfter else { throw AnnotationError.notAuthorized }
        }
        let component = try annotationComponent(draft, epoch: epoch, sequence: sequence)
        let expiry = draft.authoredAtUnixMS + 86_400_000
        let bytes = annotationSigningBytes(component: component, grant: canonicalGrant, authored: draft.authoredAtUnixMS, expiry: expiry)
        let signature = try key.signature(for: bytes)
        let body = GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant", canonicalGrant), ("component", component),
            ("signed_at_unix_ms", Data(String(draft.authoredAtUnixMS).utf8)), ("expires_at_unix_ms", Data(String(expiry).utf8)),
            ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))
        ])
        guard body.count <= 64 << 10 else { throw AnnotationError.invalidRecord }
        return body
    }

    func validateAnnotation(_ body: Data, draft: AnnotationDraft) throws {
        guard canAnnotate, draft.authorPrincipal == principal, body.count <= 64 << 10 else { throw AnnotationError.invalidRecord }
        let record = try JSONValue.decode(body, limit: 64 << 10)
        guard let fields = record.object, Set(fields.keys) == ["version", "grant", "component", "signed_at_unix_ms", "expires_at_unix_ms", "signature"],
              record["version"]?.int == 1, let oldGrant = record["grant"],
              oldGrant["grant"]?["public_key"] == grant["grant"]?["public_key"],
              oldGrant["grant"]?["source"] == grant["grant"]?["source"],
              oldGrant["grant"]?["principal"]?.string == principal,
              oldGrant["grant"]?["tenant"]?.string == tenant, oldGrant["grant"]?["site"]?.string == site,
              oldGrant["grant"]?["domain"]?.string == "simulation",
              oldGrant["grant"]?["kind"]?.string == "operator",
              oldGrant["grant"]?["publish_audience"]?.string == audience,
              oldGrant["grant"]?["scopes"]?.array?.contains(.string("annotation-write")) == true,
              let epoch = record["component"]?["version"]?["epoch"]?.uint,
              let sequence = record["component"]?["version"]?["sequence"]?.uint,
              record["signed_at_unix_ms"]?.int == draft.authoredAtUnixMS,
              record["expires_at_unix_ms"]?.int == draft.authoredAtUnixMS + 86_400_000,
              let signature = record["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw AnnotationError.invalidRecord }
        let component = try annotationComponent(draft, epoch: epoch, sequence: sequence)
        guard record["component"] == (try JSONValue.decode(component)) else { throw AnnotationError.invalidRecord }
        let oldCanonical = try GoAuthorityJSON.signedGrant(oldGrant)
        let bytes = annotationSigningBytes(component: component, grant: oldCanonical, authored: draft.authoredAtUnixMS, expiry: draft.authoredAtUnixMS + 86_400_000)
        guard key.publicKey.isValidSignature(signature, for: bytes) else { throw AnnotationError.invalidRecord }
    }

    func annotationSigningBytes(component: Data, grant: Data, authored: Int64, expiry: Int64) -> Data {
        var bytes = Data("wendy.world.authority.v1/record\0".utf8)
        bytes.append(GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(Self.digest(grant))),
            ("component", component), ("signed_at", Data(String(authored).utf8)), ("expires_at", Data(String(expiry).utf8))
        ]))
        return bytes
    }

    func annotationComponent(_ draft: AnnotationDraft, epoch: UInt64, sequence: UInt64) throws -> Data {
        try draft.validate()
        guard draft.authorPrincipal == principal, epoch > 0, sequence > 0 else { throw AnnotationError.invalidRecord }
        func s(_ value: String) -> Data { GoAuthorityJSON.string(value) }
        func n<T: BinaryInteger>(_ value: T) -> Data { Data(String(value).utf8) }
        let o = GoAuthorityJSON.object
        let clock = "simulation:" + draft.context.runID
        var payloadFields: [(String, Data)] = [
            ("id", s(draft.id)), ("author_principal", s(principal)), ("text", s(draft.text)),
            ("position_mm", Data(("[" + draft.positionMM.map(String.init).joined(separator: ",") + "]").utf8)),
            ("authored_at_unix_ms", n(draft.authoredAtUnixMS)), ("expires_at_unix_ms", n(draft.expiresAtUnixMS))
        ]
        var errorBound: Int64 = 1_000_000_000
        if draft.schemaVersion == 2 {
            guard let evidence = draft.clock else { throw AnnotationClockError.unavailable }
            try evidence.verify(credentials: self)
            let interval = try evidence.interval()
            errorBound = (interval.upperUnixMS - interval.lowerUnixMS) * 1_000_000
            payloadFields.append(("clock", try evidence.canonical()))
        }
        let payload = o(payloadFields)
        return o([
            ("key", o([("tenant", s(tenant)), ("site", s(site)), ("entity", s("annotation/" + draft.id)), ("type", s(draft.schemaVersion == 2 ? "wendy.annotation.note.v2" : "wendy.annotation.note.v1")), ("source", try GoAuthorityJSON.source(grant["grant"]?["source"]))])),
            ("version", o([("epoch", n(epoch)), ("sequence", n(sequence))])),
            ("frame", o([("id", s(draft.context.frameID)), ("revision", s(draft.context.frameRevision))])),
            ("timing", o([("sample_time", s(Self.annotationTime(draft.authoredAtUnixMS))), ("clock_domain", s(clock)), ("error_known", Data("true".utf8)), ("error_bound_ns", n(errorBound)), ("age_hint_ns", n(0))])),
            ("ttl_ns", n((draft.expiresAtUnixMS - draft.authoredAtUnixMS) * 1_000_000)),
            ("provenance", o([("origin", s("authored")), ("app_version", s(draft.schemaVersion == 2 ? "world-annotations-v2" : "world-annotations-v1")), ("simulation", o([("run_id", s(draft.context.runID)), ("scenario_version", s(draft.context.frameRevision)), ("seed", n(0)), ("clock_domain", s(clock))]))])),
            ("payload", payload), ("tombstone", Data("false".utf8))
        ])
    }

    static func annotationTime(_ millis: Int64) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.formatOptions = [.withInternetDateTime]
        let whole = formatter.string(from: Date(timeIntervalSince1970: Double(millis / 1000)))
        let fraction = millis % 1000
        guard fraction != 0 else { return whole }
        var digits = String(format: "%03lld", fraction)
        while digits.last == "0" { digits.removeLast() }
        return String(whole.dropLast()) + "." + digits + "Z"
    }
}

extension GoAuthorityJSON {
    static func source(_ value: JSONValue?) throws -> Data {
        guard let source = value?.object else { throw AnnotationError.invalidRecord }
        return try object(["device", "app", "service", "stream"].map { name in
            guard let text = source[name]?.string, !text.isEmpty else { throw AnnotationError.invalidRecord }
            return (name, string(text))
        })
    }
}
