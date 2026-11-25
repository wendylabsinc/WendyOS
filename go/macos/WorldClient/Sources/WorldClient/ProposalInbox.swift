import CryptoKit
import Foundation

public enum ProposalInboxError: Error, LocalizedError {
    case invalid, changed, capacity, wrongOrigin
    public var errorDescription: String? {
        switch self {
        case .invalid: "The proposal history, scope or immutable proof is invalid."
        case .changed: "The inbox changed during this read. The previous complete history was kept."
        case .capacity: "The complete proposal history exceeds this device's bounded inbox."
        case .wrongOrigin: "Import the explicitly configured mission profile for this exact proposal origin before reviewing it."
        }
    }
}

/// Public trust configuration, never an endpoint discovered from a receipt.
public struct ProposalInboxOrigin: Codable, Sendable, Equatable, Identifiable {
    public let origin, principal, assignee, runID: String
    public let rootPublicKey: Data
    public var id: String { origin }
    enum CodingKeys: String, CodingKey { case origin, principal, assignee; case runID = "run_id", rootPublicKey = "root_public_key" }
    public init(json: JSONValue) throws {
        guard Set(json.object?.keys.map { $0 } ?? []) == ["origin", "principal", "assignee", "run_id", "root_public_key"],
              let origin = json["origin"]?.string, let principal = json["principal"]?.string,
              let assignee = json["assignee"]?.string, let run = json["run_id"]?.string,
              let root = json["root_public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), root.count == 32,
              [origin, run].allSatisfy(Self.identifier), [principal, assignee].allSatisfy({ !$0.isEmpty && $0.utf8.count <= 256 }) else { throw ProposalInboxError.invalid }
        self.origin = origin; self.principal = principal; self.assignee = assignee; runID = run; rootPublicKey = root
    }
    public static func profile(_ value: JSONValue?) throws -> [Self] {
        guard let value else { return [] }
        guard Set(value.object?.keys.map { $0 } ?? []) == ["version", "origins"], value["version"]?.int == 1,
              let rows = value["origins"]?.array, !rows.isEmpty, rows.count <= 8 else { throw ProposalInboxError.invalid }
        let out = try rows.map(Self.init(json:))
        guard Set(out.map(\.origin)).count == out.count else { throw ProposalInboxError.invalid }
        return out.sorted { $0.origin < $1.origin }
    }
    public func validateMissionOrigin(credentials: MissionCredentials, context: MissionContext) throws {
        guard credentials.audience == origin, context.rootPublicKey == rootPublicKey, context.runID == runID, context.assignee == assignee else { throw ProposalInboxError.wrongOrigin }
    }
    static func identifier(_ value: String) -> Bool { (1...128).contains(value.utf8.count) && value.utf8.allSatisfy { (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0 == 45 || $0 == 95 } }
}

public struct RemoteProposal: Sendable, Identifiable {
    public let raw: Data
    public let record: JSONValue
    public let origin: ProposalInboxOrigin
    public let proposalID, bodySHA256, summary, principal, type: String
    public let sequence: UInt64
    public let expiresAtUnixMS, acceptedAtUnixMS, exportedAtUnixMS: Int64
    public let gatewayOriginVerification, gatewayProposerVerification: String
    public var id: String { origin.origin + ":" + proposalID }
    public init(data: Data, origin: ProposalInboxOrigin, tenant: String, site: String) throws {
        _ = try MissionRawJSON.field(data, path: [], limit: 128 << 10)
        let v = try JSONValue.decode(data, limit: 128 << 10)
        guard v["origin"]?.string == origin.origin, v["executable"]?.bool == false, v["historical_provenance_verified"]?.bool == true,
              let r = v["record"], let p = r["proposal"], let proof = r["proof"], let originProof = v["origin_proof"],
              Set(r.object?.keys.map { $0 } ?? []) == ["sequence", "proposal", "principal", "kind", "source", "grant_id", "class", "accepted_at_unix_ms", "body_sha256", "body", "proof"],
              Set(p.object?.keys.map { $0 } ?? []) == ["version", "id", "tenant", "site", "run_id", "assignee", "type", "parameters", "summary", "not_after_unix_ms"],
              let body = r["body"]?.string.flatMap({ Data(base64Encoded: $0) }), body.count <= 20 << 10,
              r["body_sha256"]?.string == WorldCredentials.digest(body), try JSONValue.decode(body, limit: 32 << 10) == p,
              p["version"]?.int == 1, p["tenant"]?.string == tenant, p["site"]?.string == site,
              p["run_id"]?.string == origin.runID, p["assignee"]?.string == origin.assignee,
              let id = p["id"]?.string, ProposalInboxOrigin.identifier(id), let sequence = r["sequence"]?.uint, sequence > 0,
              let principal = r["principal"]?.string, !principal.isEmpty, let summary = p["summary"]?.string, summary.utf8.count <= 512,
              let type = p["type"]?.string, !type.isEmpty, type.utf8.count <= 256, ["OBSERVE", "MOVE", "ACTUATE"].contains(r["class"]?.string ?? ""),
              let accepted = r["accepted_at_unix_ms"]?.int, accepted > 0,
              let expiry = p["not_after_unix_ms"]?.int, expiry > accepted, expiry - accepted <= 86_400_000,
              let issued = proof["issued_at_unix_ms"]?.int, let requestEnd = proof["expires_at_unix_ms"]?.int,
              accepted >= issued, accepted < requestEnd,
              let exported = originProof["issued_at_unix_ms"]?.int, exported >= accepted,
              let originState = v["origin_verification"]?.string, let proposerState = v["proposer_verification"]?.string,
              [originState, proposerState].allSatisfy({ ["current_grant_verified", "not_currently_verified", "revocation_state_unavailable", "grant_expired"].contains($0) }) else { throw ProposalInboxError.invalid }
        _ = try MissionRawJSON.field(body, path: [], limit: 32 << 10)
        guard p["parameters"]?.object != nil, try MissionRawJSON.field(body, path: ["parameters"], limit: 32 << 10).count <= 16 << 10 else { throw ProposalInboxError.invalid }
        let grant = try Self.verify(proof, body: body, operation: "propose", origin: origin, tenant: tenant, site: site)
        guard let grantBefore = grant["not_before_unix_ms"]?.int, let grantEnd = grant["not_after_unix_ms"]?.int, accepted >= grantBefore, accepted < grantEnd, expiry <= grantEnd, r["principal"] == grant["principal"], r["kind"] == grant["kind"], r["source"] == grant["source"], r["grant_id"] == grant["id"] else { throw ProposalInboxError.invalid }
        // Go's signed attestation includes this exact compact record, including
        // its original request bytes and sequence. A reordered outer response
        // is harmless; changing/re-encoding the attested record fails closed.
        let recordBytes = try MissionRawJSON.field(data, path: ["record"], limit: 128 << 10, requireCompact: true)
        let attestation = GoAuthorityJSON.object([("purpose", GoAuthorityJSON.string("wendy.world.proposal-history.v1")), ("origin", GoAuthorityJSON.string(origin.origin)), ("record", recordBytes)])
        let originGrant = try Self.verify(originProof, body: attestation, operation: "replicate", origin: origin, tenant: tenant, site: site)
        guard originGrant["principal"]?.string == origin.principal else { throw ProposalInboxError.invalid }
        self.raw = data; record = r; self.origin = origin; proposalID = id; bodySHA256 = WorldCredentials.digest(body)
        self.summary = summary; self.principal = principal; self.type = type; self.sequence = sequence
        expiresAtUnixMS = expiry; acceptedAtUnixMS = accepted; exportedAtUnixMS = exported
        gatewayOriginVerification = originState; gatewayProposerVerification = proposerState
    }
    /// This is historical signature verification. It cannot establish either
    /// current permission or revocation knowledge at the original event time.
    private static func verify(_ proof: JSONValue, body: Data, operation: String, origin: ProposalInboxOrigin, tenant: String, site: String) throws -> JSONValue {
        guard Set(proof.object?.keys.map { $0 } ?? []) == ["version", "grant", "audience", "operation", "body_sha256", "nonce", "issued_at_unix_ms", "expires_at_unix_ms", "signature"],
              proof["version"]?.int == 1, proof["audience"]?.string == origin.origin, proof["operation"]?.string == operation,
              proof["body_sha256"]?.string == WorldCredentials.digest(body), let nonce = proof["nonce"]?.string, nonce.utf8.count >= 16, ProposalInboxOrigin.identifier(nonce),
              let issued = proof["issued_at_unix_ms"]?.int, let expires = proof["expires_at_unix_ms"]?.int, issued > 0, issued <= Int64(Date().timeIntervalSince1970 * 1000), expires > issued, expires - issued <= 60_000,
              let signed = proof["grant"], Set(signed.object?.keys.map { $0 } ?? []) == ["grant", "signature"], let g = signed["grant"],
              g["version"]?.int == 1, g["tenant"]?.string == tenant, g["site"]?.string == site, g["domain"]?.string == "simulation",
              g["scopes"]?.array?.contains(.string(operation)) == true,
              operation == "propose" ? ["agent", "operator"].contains(g["kind"]?.string ?? "") : g["kind"]?.string == "device",
              let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int, before > 0, after > before, after - before <= 31_536_000_000, issued >= before, issued < after,
              let key = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), key.count == 32,
              let grantSignature = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }), grantSignature.count == 64,
              let signature = proof["signature"]?.string.flatMap({ Data(base64Encoded: $0) }), signature.count == 64 else { throw ProposalInboxError.invalid }
        let canonical = try GoAuthorityJSON.signedGrant(signed)
        guard let marker = canonical.range(of: Data(",\"signature\":".utf8), options: .backwards) else { throw ProposalInboxError.invalid }
        var grantMessage = Data("wendy.world.authority.v1/grant\0".utf8)
        grantMessage.append(canonical[canonical.index(canonical.startIndex, offsetBy: 9)..<marker.lowerBound])
        guard try Curve25519.Signing.PublicKey(rawRepresentation: origin.rootPublicKey).isValidSignature(grantSignature, for: grantMessage) else { throw ProposalInboxError.invalid }
        var request = Data("wendy.world.authority.v1/request\0".utf8)
        request.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(canonical))), ("audience", GoAuthorityJSON.string(origin.origin)), ("operation", GoAuthorityJSON.string(operation)), ("body_digest", GoAuthorityJSON.string(WorldCredentials.digest(body))), ("nonce", GoAuthorityJSON.string(nonce)), ("issued", Data(String(issued).utf8)), ("expires", Data(String(expires).utf8))]))
        guard try Curve25519.Signing.PublicKey(rawRepresentation: key).isValidSignature(signature, for: request) else { throw ProposalInboxError.invalid }
        return g
    }
    public func matchesLive(_ live: MissionProposal, credentials: MissionCredentials, context: MissionContext) throws {
        try origin.validateMissionOrigin(credentials: credentials, context: context)
        guard live.reference.audience == origin.origin, live.id == proposalID, live.reference.bodySHA256 == bodySHA256 else { throw ProposalInboxError.wrongOrigin }
    }
}

public struct ProposalInboxDocument: Codable, Sendable {
    public let origin: ProposalInboxOrigin
    public let pages: [Data]
    public let fetchedUnixMS: Int64
    public let scopeID: String
    public func rows(tenant: String, site: String) throws -> [RemoteProposal] {
        guard !pages.isEmpty, pages.count <= 16, pages.reduce(0, { $0 + $1.count }) <= 2 << 20 else { throw ProposalInboxError.capacity }
        var out: [RemoteProposal] = [], previous = "", through: UInt64?, sequences = Set<UInt64>()
        for (index, bytes) in pages.enumerated() {
            let page = try ProposalInboxPage(bytes, origin: origin, tenant: tenant, site: site)
            if let through { guard through == page.through else { throw ProposalInboxError.changed } } else { through = page.through }
            for row in page.rows { guard row.proposalID > previous, sequences.insert(row.sequence).inserted else { throw ProposalInboxError.changed }; previous = row.proposalID; out.append(row) }
            guard index == pages.count - 1 ? page.next.isEmpty : (!page.rows.isEmpty && page.next == previous) else { throw ProposalInboxError.changed }
        }
        guard out.count <= 256 else { throw ProposalInboxError.capacity }
        guard UInt64(out.count) == through, sequences == Set((0..<out.count).map { UInt64($0 + 1) }) else { throw ProposalInboxError.changed }; return out
    }
}

struct ProposalInboxPage {
    let rows: [RemoteProposal], next: String, through: UInt64
    init(_ data: Data, origin: ProposalInboxOrigin, tenant: String, site: String) throws {
        _ = try MissionRawJSON.field(data, path: [], limit: 1 << 20)
        let v = try JSONValue.decode(data, limit: 1 << 20)
        guard v["origin"]?.string == origin.origin, let next = v["next"]?.string, next.isEmpty || ProposalInboxOrigin.identifier(next),
              let progress = v["progress"], let through = progress["through"]?.uint, let sync = progress["last_sync_unix_ms"]?.int, sync >= 0,
              progress["via"]?.string != nil, progress["caught_up_with_peer"]?.bool != nil else { throw ProposalInboxError.invalid }
        let raw = try MissionRawJSON.field(data, path: ["proposals"], limit: 1 << 20)
        let elements = try Self.elements(raw)
        let rows = try elements.map { try RemoteProposal(data: $0, origin: origin, tenant: tenant, site: site) }
        guard rows.count <= 100, rows.allSatisfy({ $0.sequence <= through }) else { throw ProposalInboxError.invalid }
        self.rows = rows; self.next = next; self.through = through
    }
    private static func elements(_ data: Data) throws -> [Data] {
        guard let array = try JSONValue.decode(data, limit: 1 << 20).array, array.count <= 100 else { throw ProposalInboxError.invalid }
        let bytes = Array(data); var depth = 0, quoted = false, escape = false, start: Int?, out: [Data] = []
        for (i, b) in bytes.enumerated() {
            if quoted { if escape { escape = false } else if b == 92 { escape = true } else if b == 34 { quoted = false }; continue }
            if b == 34 { quoted = true }
            if b == 123 || b == 91 { if depth == 1 { start = i }; depth += 1 }
            if b == 125 || b == 93 { depth -= 1; if depth == 1, let s = start { out.append(Data(bytes[s...i])); start = nil } }
        }
        guard out.count == array.count else { throw ProposalInboxError.invalid }; return out
    }
}

public actor ProposalInboxClient {
    public nonisolated let credentials: WorldCredentials
    private var last: Int64 = 0
    public init(credentials: WorldCredentials) { self.credentials = credentials }
    public func read(origin: ProposalInboxOrigin, pageSize: Int = 32) async throws -> ProposalInboxDocument {
        guard (1...100).contains(pageSize) else { throw ProposalInboxError.invalid }
        var pages: [Data] = [], cursor = "", through: UInt64?
        repeat {
            try Task.checkCancellation()
            let now = try current()
            var url = URLComponents(url: credentials.endpoint.appendingPathComponent("v1/proposal-inbox"), resolvingAgainstBaseURL: false)!
            url.queryItems = [URLQueryItem(name: "origin", value: origin.origin), URLQueryItem(name: "limit", value: String(pageSize))]
            if !cursor.isEmpty { url.queryItems?.append(URLQueryItem(name: "after", value: cursor)) }
            var request = URLRequest(url: url.url!, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 3)
            request.setValue(try credentials.signedReadAuthorization(operation: "read", nonce: UUID().uuidString, issuedAtUnixMS: now).base64EncodedString(), forHTTPHeaderField: "X-World-Authorization")
            let bytes = try await BoundedRequest(limit: 1 << 20, resourceTimeout: 3).run(request)
            _ = try current()
            let page = try ProposalInboxPage(bytes, origin: origin, tenant: credentials.tenant, site: credentials.site)
            if let through { guard through == page.through else { throw ProposalInboxError.changed } } else { through = page.through }
            guard page.next.isEmpty || page.next > cursor else { throw ProposalInboxError.changed }
            cursor = page.next; pages.append(bytes)
            guard pages.count <= 16, pages.reduce(0, { $0 + $1.count }) <= 2 << 20 else { throw ProposalInboxError.capacity }
        } while !cursor.isEmpty
        let document = ProposalInboxDocument(origin: origin, pages: pages, fetchedUnixMS: try current(), scopeID: credentials.scopeID)
        _ = try document.rows(tenant: credentials.tenant, site: credentials.site); _ = try current()
        return document
    }
    private func current() throws -> Int64 {
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        guard now >= last else { throw ProposalInboxError.invalid }; last = now
        guard now >= credentials.notBefore, now < credentials.notAfter else { throw WorldClientError.expiredCredentials }; return now
    }
}

public actor ProposalInboxJournal {
    private let file: MissionFile
    private let credentials: WorldCredentials
    private let origins: [ProposalInboxOrigin]
    private var saved: [ProposalInboxDocument] = []
    private var digest: String?
    public init(directory: URL, credentials: WorldCredentials, origins: [ProposalInboxOrigin]) throws {
        guard !origins.isEmpty, origins.count <= 8, Set(origins.map(\.origin)).count == origins.count else { throw ProposalInboxError.invalid }
        self.credentials = credentials; self.origins = origins
        let encoder = JSONEncoder(); encoder.outputFormatting = [.sortedKeys]
        let identity = try encoder.encode(origins.sorted { $0.origin < $1.origin })
        file = MissionFile(directory: directory, scopeID: WorldCredentials.digest(Data(("proposal-inbox-v1:" + credentials.scopeID).utf8) + identity))
        if let data = try file.load() {
            let docs = try JSONDecoder().decode([ProposalInboxDocument].self, from: data)
            guard docs.count <= origins.count, Set(docs.map { $0.origin.origin }).count == docs.count else { throw ProposalInboxError.invalid }
            for doc in docs { guard origins.contains(doc.origin), doc.scopeID == credentials.scopeID else { throw ProposalInboxError.invalid }; _ = try doc.rows(tenant: credentials.tenant, site: credentials.site) }
            saved = docs; digest = WorldCredentials.digest(data)
        }
    }
    public func documents() -> [ProposalInboxDocument] { saved }
    public func install(_ doc: ProposalInboxDocument) throws {
        guard origins.contains(doc.origin), doc.scopeID == credentials.scopeID else { throw ProposalInboxError.invalid }
        let rows = try doc.rows(tenant: credentials.tenant, site: credentials.site)
        if let old = saved.first(where: { $0.origin == doc.origin }) {
            let previous = try old.rows(tenant: credentials.tenant, site: credentials.site)
            for item in previous { guard rows.contains(where: { $0.proposalID == item.proposalID && $0.record == item.record }) else { throw ProposalInboxError.changed } }
        }
        var next = saved.filter { $0.origin != doc.origin }; next.append(doc); next.sort { $0.origin.origin < $1.origin.origin }
        let data = try JSONEncoder().encode(next); guard data.count <= 4 << 20 else { throw ProposalInboxError.capacity }
        try file.save(data, replacing: digest); saved = next; digest = WorldCredentials.digest(data)
    }
}

extension MissionProposal {
    public var canDraftWithoutPreparedHandoff: Bool {
        triggerSHA256 == nil && inspectionDestination == nil && target.count == 3 && (-2...2).contains(target[0]) && abs(target[1] - 0.2) <= 1e-9 && abs(target[2]) <= 1e-9
    }
    /// Copies the exact reviewed vector. A trigger-bearing proposal still needs
    /// its existing prepared trigger and cannot be converted by digest alone.
    public func unsignedDraft(id: String = "native-" + UUID().uuidString.lowercased(), durationMS: Int64, context: MissionContext, prepared: PreparedProposalHandoff? = nil) throws -> MissionDraft {
        if let prepared {
            guard prepared.proposal == reference, prepared.notAfterUnixMS > Int64(Date().timeIntervalSince1970 * 1000) else { throw MissionError.expired }
            return try MissionDraft(id: id, target: target, durationMS: durationMS, context: context, proposal: self,
                                    trigger: prepared.trigger, inspectionDestination: prepared.destination, preparedUntilUnixMS: prepared.notAfterUnixMS, preparedHandoff: prepared.raw)
        }
        guard canDraftWithoutPreparedHandoff else { throw MissionError.invalidDraft }
        return try MissionDraft(id: id, target: target, durationMS: durationMS, context: context, proposal: self)
    }
}

extension MissionNavigationProfile {
    public static func imported(_ value: JSONValue?) throws -> MissionNavigationProfile {
        guard let value else { return .oneAxis }
        if value == .object(["kind": .string("one_axis")]) { return .oneAxis }
        guard Set(value.object?.keys.map { $0 } ?? []) == ["kind", "floor_y"], value["kind"]?.string == "planar",
              let floor = value["floor_y"]?.double, floor.isFinite, abs(floor) <= 1_000_000 else { throw MissionError.invalidProfile }
        return .planar(floorY: floor)
    }
}

/// Explicit local handoff from an already verified/prepared reader. This checks
/// exact correspondence and deadlines, not sensor truth or producer signatures.
public struct PreparedProposalHandoff: Sendable {
    public let proposal: MissionProposalRef
    public let trigger: MissionNativeTrigger
    public let destination: MissionInspectionDestination?
    public let notAfterUnixMS: Int64
    public let fileSHA256: String
    public let raw: Data
    public init(data: Data, proposal: MissionProposal, context: MissionContext, nowUnixMS: Int64 = Int64(Date().timeIntervalSince1970 * 1000)) throws {
        _ = try MissionRawJSON.field(data, path: [], limit: 512 << 10)
        let v = try JSONValue.decode(data, limit: 512 << 10)
        let required: Set<String> = ["version", "proposal_id", "mission_input", "original_envelope_sha256"]
        let optional: Set<String> = ["navigation_target", "inspection_destination", "route_length_m", "nominal_travel_ms", "minimum_duration_ms", "navigation", "envelope_json", "request_id"]
        guard let fields = v.object, required.isSubset(of: Set(fields.keys)), Set(fields.keys).isSubset(of: required.union(optional)),
              v["version"]?.int == 1, v["proposal_id"]?.string == proposal.id, let input = v["mission_input"], Self.validInput(input),
              let raw = input["trigger_json"]?.string.flatMap({ Data(base64Encoded: $0) }), let envelope = input["envelope_sha256"]?.string,
              v["original_envelope_sha256"]?.string == envelope else { throw MissionError.invalidDraft }
        let trigger = try MissionNativeTrigger(canonical: raw, fixedTarget: MissionWire.vector(input["target"]), originalEnvelopeSHA256: envelope)
        guard proposal.triggerSHA256 == trigger.sha256, input["trigger_sha256"]?.string == trigger.sha256,
              input["result_schema"]?.string == trigger.resultSchema else { throw MissionError.invalidDraft }
        let destination = try v["inspection_destination"].map(MissionInspectionDestination.init(json:))
        guard destination == proposal.inspectionDestination else { throw MissionError.invalidDraft }
        if destination != nil { guard try MissionWire.vector(v["navigation_target"]) == proposal.target else { throw MissionError.invalidDraft } }
        else { guard v["navigation_target"] == nil, trigger.fixedTarget == proposal.target else { throw MissionError.invalidDraft } }
        if let encoded = v["envelope_json"] {
            guard let bytes = encoded.string.flatMap({ Data(base64Encoded: $0) }), WorldCredentials.digest(bytes) == envelope else { throw MissionError.invalidDraft }
        }
        let deadline = try Self.deadline(input: input, trigger: trigger)
        guard deadline > nowUnixMS else { throw MissionError.expired }
        _ = try MissionDraft(target: proposal.target, durationMS: min(context.maxDurationMS, 30_000), context: context, proposal: proposal,
                             trigger: trigger, inspectionDestination: destination, preparedUntilUnixMS: deadline, preparedHandoff: data)
        self.proposal = proposal.reference; self.trigger = trigger; self.destination = destination
        notAfterUnixMS = deadline; fileSHA256 = WorldCredentials.digest(data); self.raw = data
    }
    private static func validInput(_ input: JSONValue) -> Bool {
        Set(input.object?.keys.map { $0 } ?? []) == ["version", "trigger_json", "trigger_sha256", "target", "envelope_sha256", "result_schema", "sample_valid_until", "physical_valid_until"] && input["version"]?.int == 1
    }
    static func unixMilliseconds(_ value: String?) throws -> Int64 {
        try timestampParts(value).milliseconds
    }
    private static func timestampParts(_ value: String?) throws -> (milliseconds: Int64, remainderNS: Int64) {
        guard let value, value.utf8.count <= 64, AreaTime.parse(value) != nil else { throw MissionError.invalidDraft }
        let pattern = #"^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.([0-9]{1,9}))?(Z|[+-][0-9]{2}:[0-9]{2})$"#
        let regex = try NSRegularExpression(pattern: pattern), text = value as NSString
        guard let match = regex.firstMatch(in: value, range: NSRange(location: 0, length: text.length)),
              let whole = AreaTime.parse(text.substring(with: match.range(at: 1)) + text.substring(with: match.range(at: 3))),
              whole.timeIntervalSince1970 > 0, whole.timeIntervalSince1970 < 9e12 else { throw MissionError.invalidDraft }
        let fraction = match.range(at: 2).location == NSNotFound ? "" : text.substring(with: match.range(at: 2))
        let nanoseconds = Int64(String((fraction + "000000000").prefix(9)))!
        let (wholeMS, overflow) = Int64(whole.timeIntervalSince1970.rounded(.down)).multipliedReportingOverflow(by: 1000)
        let (milliseconds, fractionOverflow) = wholeMS.addingReportingOverflow(nanoseconds / 1_000_000)
        guard !overflow, !fractionOverflow else { throw MissionError.invalidDraft }
        return (milliseconds, nanoseconds % 1_000_000)
    }
    static func deadline(input: JSONValue, trigger: MissionNativeTrigger) throws -> Int64 {
        let component = try JSONValue.decode(trigger.canonical)["component"]!
        let parts = try timestampParts(trigger.sampleTime), sample = parts.milliseconds
        guard let ttl = component["ttl_ns"]?.int, ttl > 0, ttl < 86_400_000_000_000,
              let error = component["timing"]?["error_bound_ns"]?.int, error >= 0, error < ttl else { throw MissionError.invalidDraft }
        // Sample timestamps belong to the declared simulation clock. They are
        // bounds to validate, never a Unix clock anchor or a renewable duration.
        let sampleUntil = try unixMilliseconds(input["sample_valid_until"]?.string)
        // Add the original sub-millisecond remainder before flooring once.
        // Flooring sample and lifetime separately can reject a valid carry.
        let (remainingNS, durationOverflow) = (ttl - error).addingReportingOverflow(parts.remainderNS)
        let (originalCap, capOverflow) = sample.addingReportingOverflow(remainingNS / 1_000_000)
        guard !durationOverflow, !capOverflow else { throw MissionError.invalidDraft }
        var sampleCap = originalCap
        var deadline = try unixMilliseconds(input["physical_valid_until"]?.string)
        if let bytes = component["payload"]?["result_json"]?.string.flatMap({ Data(base64Encoded: $0) }) {
            let result = try JSONValue.decode(bytes, limit: 64 << 10)
            sampleCap = min(sampleCap, try unixMilliseconds(result["valid_until"]?.string))
            if trigger.resultSchema == "wendy.person.track.v1" {
                guard let current = result["current"]?.array, !current.isEmpty else { throw MissionError.invalidDraft }
                for member in current + (result["previous"]?.array ?? []) {
                    deadline = min(deadline, try unixMilliseconds(member["physical_until"]?.string))
                }
            } else if let physical = result["physical_valid_until"]?.string {
                deadline = min(deadline, try unixMilliseconds(physical))
            }
        } else if let original = component["payload"]?["valid_until"]?.string {
            sampleCap = min(sampleCap, try unixMilliseconds(original))
        }
        guard sampleUntil > sample, sampleUntil <= sampleCap else { throw MissionError.invalidDraft }
        // The prepared reader supplied the physical bound after checking the
        // original record, delegation and device. This file importer does not
        // independently reverify those proofs or observe the simulation clock.

        return deadline
    }
    static func validateStored(_ draft: MissionDraft) throws {
        if draft.preparationMode == .legacyTrusted {
            guard draft.preparedHandoff == nil, draft.preparedUntilUnixMS == nil else { throw MissionError.invalidDraft }; return
        }
        guard let raw = draft.preparedHandoff, draft.preparedUntilUnixMS != nil else { throw MissionError.invalidDraft }
        _ = try MissionRawJSON.field(raw, path: [], limit: 512 << 10)
        let value = try JSONValue.decode(raw, limit: 512 << 10)
        guard let trigger = draft.trigger, let input = value["mission_input"], validInput(input), value["version"]?.int == 1,
              value["proposal_id"]?.string == draft.proposal?.id, input["trigger_json"]?.string.flatMap({ Data(base64Encoded: $0) }) == trigger.canonical,
              input["envelope_sha256"]?.string == trigger.originalEnvelopeSHA256,
              value["original_envelope_sha256"]?.string == trigger.originalEnvelopeSHA256,
              input["trigger_sha256"]?.string == trigger.sha256, input["result_schema"]?.string == trigger.resultSchema,
              try MissionWire.vector(input["target"]) == trigger.fixedTarget,
              try deadline(input: input, trigger: trigger) == draft.preparedUntilUnixMS else { throw MissionError.invalidDraft }
        guard try value["inspection_destination"].map(MissionInspectionDestination.init(json:)) == draft.inspectionDestination else { throw MissionError.invalidDraft }
        if draft.inspectionDestination != nil { guard try MissionWire.vector(value["navigation_target"]) == draft.target else { throw MissionError.invalidDraft } }
        else { guard trigger.fixedTarget == draft.target else { throw MissionError.invalidDraft } }
    }

}
