import Foundation
import CryptoKit

public struct AnnotationAmendmentDraft: Codable, Sendable, Equatable {
    public let original: AnnotationEntry
    public let predecessor: Data?
    public let action: String
    public let text: String
    public let positionMM: [Int64]
    public let clock: AnnotationClockEvidence
    public var expiresAtUnixMS: Int64 { original.draft.expiresAtUnixMS }
    public var originalSHA256: String { original.signedRecord.map(WorldCredentials.digest) ?? "" }
}
public struct AnnotationAmendmentEntry: Codable, Sendable, Equatable, Identifiable {
    public let draft: AnnotationAmendmentDraft
    public let signedRecord: Data
    public internal(set) var state: AnnotationState
    public internal(set) var issue: String?
    public var id: String { WorldCredentials.digest(signedRecord) }
    public var amendmentSHA256: String { (try? JSONValue.decode(signedRecord)["component"]?["key"]?["entity"]?.string?.replacingOccurrences(of: "annotation-amendment/", with: "")) ?? "" }
}

extension WorldCredentials {
    func amendmentComponent(_ draft: AnnotationAmendmentDraft, epoch: UInt64, sequence: UInt64, depth: Int = 0, validateAncestry: Bool = true) throws -> Data {
        guard canAnnotate, epoch > 0, sequence > 0, depth < 8,
              draft.original.state == .published, let original = draft.original.signedRecord,
              original.count <= 24 << 10, draft.original.draft.authorPrincipal == principal,
              ["correct", "withdraw"].contains(draft.action), draft.positionMM.count == 3,
              draft.positionMM.allSatisfy({ (-1_000_000_000...1_000_000_000).contains($0) }) else { throw AnnotationError.invalidRecord }
        try validateAnnotation(original, draft: draft.original.draft)
        try draft.clock.verify(credentials: self)
        let interval = try draft.clock.interval()
        guard interval.lowerUnixMS >= draft.original.draft.authoredAtUnixMS, interval.upperUnixMS < draft.expiresAtUnixMS else { throw AnnotationError.invalidNote }
        if draft.action == "withdraw" {
            guard draft.text.isEmpty, draft.positionMM == draft.original.draft.positionMM else { throw AnnotationError.invalidNote }
        } else {
            guard !draft.text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, draft.text.utf8.count <= 2048 else { throw AnnotationError.invalidNote }
        }
        let o = GoAuthorityJSON.object, s = GoAuthorityJSON.string
        func n<T: BinaryInteger>(_ v: T) -> Data { Data(String(v).utf8) }
        var predecessorHash = WorldCredentials.digest(original)
        if let previous = draft.predecessor {
            if validateAncestry {
                let prior = try decodeAmendment(previous, original: draft.original, depth: depth + 1)
                guard prior.draft.action != "withdraw", try prior.draft.clock.interval().lowerUnixMS <= interval.lowerUnixMS else { throw AnnotationError.invalidRecord }
                predecessorHash = prior.amendmentSHA256
            } else {
                let raw = try JSONValue.decode(previous)
                guard raw["component"]?["payload"]?["action"]?.string != "withdraw", let authored = raw["component"]?["payload"]?["authored_at_unix_ms"]?.int, authored <= interval.lowerUnixMS,
                      let entity = raw["component"]?["key"]?["entity"]?.string, entity.hasPrefix("annotation-amendment/") else { throw AnnotationError.invalidRecord }
                predecessorHash = String(entity.dropFirst("annotation-amendment/".count))
            }
        }
        var fields: [(String, Data)] = [("original_record", original), ("original_sha256", s(WorldCredentials.digest(original))), ("predecessor_sha256", s(predecessorHash))]
        if let predecessor = draft.predecessor { fields.append(("predecessor_record", predecessor)) }
        fields += [("action", s(draft.action)), ("author_principal", s(principal)), ("text", s(draft.text)),
                   ("position_mm", Data(("[" + draft.positionMM.map(String.init).joined(separator: ",") + "]").utf8)),
                   ("authored_at_unix_ms", n(interval.lowerUnixMS)), ("expires_at_unix_ms", n(draft.expiresAtUnixMS)), ("clock", try draft.clock.canonical())]
        let payload = o(fields), context = draft.original.draft.context
        guard payload.count <= 40 << 10 else { throw AnnotationError.invalidRecord }
        return o([
            ("key", o([("tenant", s(tenant)), ("site", s(site)), ("entity", s("annotation-amendment/" + Self.digest(payload))), ("type", s("wendy.annotation.amendment.v1")), ("source", try GoAuthorityJSON.source(grant["grant"]?["source"]))])),
            ("version", o([("epoch", n(epoch)), ("sequence", n(sequence))])),
            ("frame", o([("id", s(context.frameID)), ("revision", s(context.frameRevision))])),
            ("timing", o([("sample_time", s(Self.annotationTime(interval.lowerUnixMS))), ("clock_domain", s("simulation:" + context.runID)), ("error_known", Data("true".utf8)), ("error_bound_ns", n((interval.upperUnixMS - interval.lowerUnixMS) * 1_000_000)), ("age_hint_ns", n(0))])),
            ("ttl_ns", n((draft.expiresAtUnixMS - interval.lowerUnixMS) * 1_000_000)),
            ("provenance", o([("origin", s("authored")), ("app_version", s("world-annotation-amendments-v1")), ("simulation", o([("run_id", s(context.runID)), ("scenario_version", s(context.frameRevision)), ("seed", n(0)), ("clock_domain", s("simulation:" + context.runID))]))])),
            ("payload", payload), ("tombstone", Data("false".utf8))])
    }
    func signAmendment(_ draft: AnnotationAmendmentDraft, epoch: UInt64) throws -> Data {
        let interval = try draft.clock.interval()
        guard interval.lowerUnixMS >= notBefore, interval.upperUnixMS < notAfter else { throw AnnotationError.notAuthorized }
        let component = try amendmentComponent(draft, epoch: epoch, sequence: 1)
        let signature = try key.signature(for: annotationSigningBytes(component: component, grant: canonicalGrant, authored: interval.lowerUnixMS, expiry: interval.lowerUnixMS + 86_400_000))
        let body = amendmentEnvelope(component: component, signedGrant: canonicalGrant, authored: interval.lowerUnixMS, signature: signature)
        guard body.count <= 64 << 10 else { throw AnnotationError.invalidRecord }
        return body
    }
    private func amendmentEnvelope(component: Data, signedGrant: Data, authored: Int64, signature: Data) -> Data {
        GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", signedGrant), ("component", component), ("signed_at_unix_ms", Data(String(authored).utf8)), ("expires_at_unix_ms", Data(String(authored + 86_400_000).utf8)), ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))])
    }
    func decodeAmendment(_ body: Data, original: AnnotationEntry, depth: Int = 0) throws -> AnnotationAmendmentEntry {
        guard depth < 8, body.count <= 64 << 10, let originalData = original.signedRecord else { throw AnnotationError.invalidRecord }
        let raw = try JSONValue.decode(body, limit: 64 << 10)
        guard let component = raw["component"], let payload = component["payload"], let oldGrant = raw["grant"],
              payload["original_record"] == (try JSONValue.decode(originalData)),
              let action = payload["action"]?.string, let text = payload["text"]?.string,
              let positions = payload["position_mm"]?.array, positions.count == 3,
              let clockValue = payload["clock"], let epoch = component["version"]?["epoch"]?.uint,
              let sequence = component["version"]?["sequence"]?.uint,
              oldGrant["grant"]?["public_key"] == grant["grant"]?["public_key"], oldGrant["grant"]?["source"] == grant["grant"]?["source"],
              oldGrant["grant"]?["principal"]?.string == principal, oldGrant["grant"]?["tenant"]?.string == tenant,
              oldGrant["grant"]?["site"]?.string == site, oldGrant["grant"]?["publish_audience"]?.string == audience,
              oldGrant["grant"]?["kind"]?.string == "operator", oldGrant["grant"]?["domain"]?.string == "simulation",
              oldGrant["grant"]?["scopes"]?.array?.contains(.string("annotation-write")) == true,
              let signature = raw["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw AnnotationError.invalidRecord }
        let clock = try JSONDecoder().decode(AnnotationClockEvidence.self, from: clockValue.encoded())
        var predecessor: Data?
        if let previous = payload["predecessor_record"] {
            // Reconstruct canonical record bytes recursively; JSONValue encoding
            // alone would sort keys and change the exact signed record digest.
            predecessor = try canonicalAmendmentJSON(previous, original: original, depth: depth + 1)
        }
        let coordinates = try positions.map { item -> Int64 in guard let value = item.int else { throw AnnotationError.invalidRecord }; return value }
        let draft = AnnotationAmendmentDraft(original: original, predecessor: predecessor, action: action, text: text, positionMM: coordinates, clock: clock)
        let canonical = try amendmentComponent(draft, epoch: epoch, sequence: sequence, depth: depth, validateAncestry: false)
        let issued = try clock.interval().lowerUnixMS, signedGrant = try GoAuthorityJSON.signedGrant(oldGrant)
        guard raw["version"]?.int == 1, raw["signed_at_unix_ms"]?.int == issued, raw["expires_at_unix_ms"]?.int == issued + 86_400_000,
              key.publicKey.isValidSignature(signature, for: annotationSigningBytes(component: canonical, grant: signedGrant, authored: issued, expiry: issued + 86_400_000)),
              amendmentEnvelope(component: canonical, signedGrant: signedGrant, authored: issued, signature: signature) == body else { throw AnnotationError.invalidRecord }
        return AnnotationAmendmentEntry(draft: draft, signedRecord: body, state: .published)
    }
    private func canonicalAmendmentJSON(_ raw: JSONValue, original: AnnotationEntry, depth: Int) throws -> Data {
        guard depth < 8, let c = raw["component"], let p = c["payload"], let g = raw["grant"],
              let action = p["action"]?.string, let text = p["text"]?.string, let positions = p["position_mm"]?.array,
              let clock = p["clock"], let epoch = c["version"]?["epoch"]?.uint, let sequence = c["version"]?["sequence"]?.uint,
              let signature = raw["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw AnnotationError.invalidRecord }
        let previous = try p["predecessor_record"].map { try canonicalAmendmentJSON($0, original: original, depth: depth + 1) }
        let draft = AnnotationAmendmentDraft(original: original, predecessor: previous, action: action, text: text, positionMM: try positions.map { guard let n = $0.int else { throw AnnotationError.invalidRecord }; return n }, clock: try JSONDecoder().decode(AnnotationClockEvidence.self, from: clock.encoded()))
        let body = amendmentEnvelope(component: try amendmentComponent(draft, epoch: epoch, sequence: sequence, depth: depth, validateAncestry: false), signedGrant: try GoAuthorityJSON.signedGrant(g), authored: try draft.clock.interval().lowerUnixMS, signature: signature)
        guard try JSONValue.decode(body) == raw else { throw AnnotationError.invalidRecord }
        let issued = try draft.clock.interval().lowerUnixMS
        guard g["grant"]?["public_key"] == grant["grant"]?["public_key"], g["grant"]?["source"] == grant["grant"]?["source"], g["grant"]?["principal"]?.string == principal,
              g["grant"]?["tenant"]?.string == tenant, g["grant"]?["site"]?.string == site, g["grant"]?["publish_audience"]?.string == audience,
              g["grant"]?["kind"]?.string == "operator", g["grant"]?["domain"]?.string == "simulation", g["grant"]?["scopes"]?.array?.contains(.string("annotation-write")) == true,
              let before = g["grant"]?["not_before_unix_ms"]?.int, let after = g["grant"]?["not_after_unix_ms"]?.int, before <= issued, issued < after,
              key.publicKey.isValidSignature(signature, for: annotationSigningBytes(component: try amendmentComponent(draft, epoch: epoch, sequence: sequence, depth: depth, validateAncestry: false), grant: try GoAuthorityJSON.signedGrant(g), authored: issued, expiry: issued + 86_400_000)) else { throw AnnotationError.invalidRecord }
        return body
    }
}

public actor AnnotationAmendmentOutbox {
    private let credentials: WorldCredentials
    private let context: AnnotationContext
    private let file: AnnotationFile
    private var savedDigest: String?
    private var entries: [AnnotationAmendmentEntry]
    private var busy = false
    public init(directory: URL, credentials: WorldCredentials, context: AnnotationContext) throws {
        self.credentials = credentials; self.context = context
        file = AnnotationFile(directory: directory, scopeID: WorldCredentials.digest(Data((try credentials.annotationScopeID(context: context) + ":amendments:v1").utf8)))
        if let raw = try file.load() {
            entries = try JSONDecoder().decode([AnnotationAmendmentEntry].self, from: raw)
            guard entries.count <= 64, Set(entries.map(\.id)).count == entries.count else { throw AnnotationError.invalidOutbox }
            for entry in entries {
                guard entry.draft.original.draft.context == context, [.pending,.published,.expired,.denied,.conflict,.rejected].contains(entry.state) else { throw AnnotationError.invalidOutbox }
                let checked = try credentials.decodeAmendment(entry.signedRecord, original: entry.draft.original)
                guard checked.draft == entry.draft else { throw AnnotationError.invalidOutbox }
            }
            savedDigest = WorldCredentials.digest(raw)
        } else { entries = [] }
    }
    public func snapshot() -> [AnnotationAmendmentEntry] { entries }
    private func persist(_ next: [AnnotationAmendmentEntry]) throws {
        let raw = try JSONEncoder().encode(next)
        try file.save(raw, replacing: savedDigest); savedDigest = WorldCredentials.digest(raw); entries = next
    }
    public func prepare(original: AnnotationEntry, predecessor: Data?, action: String, text: String, positionMM: [Int64], using client: WorldClient) async throws {
        guard !busy, entries.count < 64, !entries.contains(where: { $0.state == .pending }), original.draft.context == context else { throw AnnotationError.invalidOutbox }
        busy = true; defer { busy = false }
        try await client.bootstrapAnnotationClock()
        let epoch = try await client.annotationEpoch(), clock = try await client.currentClock()
        let draft = AnnotationAmendmentDraft(original: original, predecessor: predecessor, action: action, text: action == "withdraw" ? "" : text, positionMM: action == "withdraw" ? original.draft.positionMM : positionMM, clock: clock)
        let body = try credentials.signAmendment(draft, epoch: epoch)
        var next = entries; next.append(AnnotationAmendmentEntry(draft: draft, signedRecord: body, state: .pending))
        try persist(next)
    }
    public func sync(using client: WorldClient) async throws {
        guard !busy else { return }; busy = true; defer { busy = false }
        try await client.bootstrapAnnotationClock()
        for index in entries.indices where entries[index].state == .pending {
            var next = entries
            if try await client.currentClock().interval().upperUnixMS >= next[index].draft.expiresAtUnixMS { next[index].state = .expired; next[index].issue = "Original note expired; exact bytes retained."; try persist(next); continue }
            do { try await client.publishAnnotationAmendment(next[index].signedRecord, original: next[index].draft.original); next[index].state = .published; next[index].issue = nil; try persist(next) }
            catch {
                let status: Int? = if case WorldClientError.httpStatus(let code) = error { code } else { nil }
                switch status { case 401,403: next[index].state = .denied; case 409: next[index].state = .conflict; case 410: next[index].state = .expired; case 400: next[index].state = .rejected; default: break }
                next[index].issue = "Exact signed amendment retained. " + error.localizedDescription; try persist(next)
                if status == nil || (status ?? 500) >= 500 { throw error }
            }
        }
    }
}

/// This is a saved local view, not permission to author or continuing verification.
public struct AnnotationViewsCache: Sendable {
    private let file: AnnotationFile
    public init(directory: URL, credentials: WorldCredentials) {
        file = AnnotationFile(directory: directory, scopeID: WorldCredentials.digest(Data((credentials.scopeID + ":amendment-views:v1").utf8)))
    }
    public func load() throws -> [JSONValue] {
        guard let raw = try file.load() else { return [] }
        let rows = try JSONDecoder().decode([JSONValue].self, from: raw)
        guard rows.count <= 4096 else { throw AnnotationError.invalidOutbox }; return rows
    }
    public func save(_ rows: [JSONValue]) throws {
        guard rows.count <= 4096 else { throw AnnotationError.invalidOutbox }
        let old = try file.load(), raw = try JSONEncoder().encode(rows)
        try file.save(raw, replacing: old.map(WorldCredentials.digest))
    }
}

public enum AnnotationViewPresentation {
    public static func state(_ row: JSONValue, offline: Bool, reading: AnnotationClockReading? = try? AnnotationMonotonicClock.read()) -> String {
        let state = row["state"]?.string ?? "unavailable"
        guard !offline, let reading, row["display_boot_id"]?.string == reading.bootID,
              let until = row["display_until_ns"]?.int, reading.nanoseconds < until else {
            return state.contains("withdrawn") ? "historical_withdrawn" : "historical_unavailable"
        }
        return state
    }
}
