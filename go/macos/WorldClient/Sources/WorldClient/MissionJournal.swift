import CryptoKit
import Foundation

public enum MissionDeliveryState: String, Codable, Sendable { case draft, signed, sending, uncertain, delivered, terminal }
public struct MissionEntry: Codable, Sendable, Identifiable {
    public let draft: MissionDraft
    public var state: MissionDeliveryState
    public var instruction: Data?
    public var record: JSONValue?
    public var lastError: String?
    public var stopInstruction: Data?
    public var stopState: String?
    public var id: String { draft.id }
    public var instructionSHA256: String? { instruction.map(WorldCredentials.digest) }
    public var phase: String { record?["status"]?["Phase"]?.string ?? state.rawValue }
    public var terminal: Bool { ["succeeded", "failed", "canceled", "rejected"].contains(phase) }
}

public struct MissionStopEntry: Codable, Sendable, Identifiable {
    public let id: String
    public let taskID: String
    public let instruction: Data
    public var state: String
    public var record: JSONValue?
    public var originalRecordJSON: Data?
    public var error: String?
    public var instructionSHA256: String { WorldCredentials.digest(instruction) }
}

public protocol MissionTransport: Sendable {
    func beginMissionOperation() async throws -> UInt64
    func prepareInstruction(draft: MissionDraft, operation: UInt64) async throws -> Data
    func submit(instruction: Data, operation: UInt64) async throws -> JSONValue
    func status(taskID: String) async throws -> JSONValue
    func prepareStop(taskID: String) async throws -> Data
    func submitStop(instruction: Data) async throws -> JSONValue
    func submitStopRecord(instruction: Data) async throws -> (value: JSONValue, originalJSON: Data?)
}
public extension MissionTransport {
    /// Existing transports can keep their typed response. Exact HTTP bytes are
    /// needed to verify unfamiliar native payload ordering without a saved task.
    func submitStopRecord(instruction: Data) async throws -> (value: JSONValue, originalJSON: Data?) {
        (try await submitStop(instruction: instruction), nil)
    }
}
extension MissionClient: MissionTransport {}

/// No method drains this journal. Every signing, delivery and retry is called
/// by a current user action. Restart restores bytes and uncertainty only.
public actor MissionJournal {
    public nonisolated let scopeID: String
    private let credentials: MissionCredentials
    private let context: MissionContext
    private let file: MissionFile
    private var values: [MissionEntry]
    private var stops: [MissionStopEntry] = []
    private var storedDigest: String?
    private var busy: Set<String> = []
    private var stopping: Set<String> = []
    private struct Saved: Codable { let version: Int; let scopeID: String; let contextSHA256: String; var entries: [MissionEntry]; var stops: [MissionStopEntry]? }

    public init(directory: URL, credentials: MissionCredentials, context: MissionContext) throws {
        self.credentials = credentials; self.context = context
        scopeID = WorldCredentials.digest(Data((credentials.scopeID + ":" + context.digest + context.navigationProfile.journalSuffix).utf8))
        file = MissionFile(directory: directory, scopeID: scopeID)
        let saved = try file.load(); storedDigest = saved.map(WorldCredentials.digest)
        if let saved {
            let parsed = try JSONDecoder().decode(Saved.self, from: saved)
            guard (parsed.version == 1 || parsed.version == 2), parsed.scopeID == scopeID, parsed.contextSHA256 == context.digest,
                  parsed.entries.count <= 128, Set(parsed.entries.map(\.id)).count == parsed.entries.count else { throw MissionError.invalidJournal }
            if parsed.version == 2 {
                let rawEntries = try JSONValue.decode(saved)["entries"]?.array
                guard let rawEntries, rawEntries.allSatisfy({ $0["draft"]?["preparationMode"]?.string != nil }) else { throw MissionError.invalidJournal }
            }
            guard (parsed.stops?.count ?? 0) <= 128 else { throw MissionError.invalidJournal }
            stops = try (parsed.stops ?? []).map { entry in
                var restored = entry
                guard MissionWire.identifier(entry.taskID), MissionWire.identifier(entry.id) else { throw MissionError.invalidJournal }
                let instruction = try JSONValue.decode(entry.instruction)
                guard let issued = instruction["issued_at_unix_ms"]?.int, let expires = instruction["expires_at_unix_ms"]?.int else { throw MissionError.invalidJournal }
                try credentials.verifyInstruction(entry.instruction, operation: "stop", payload: GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(entry.taskID))]), issued: issued, expires: expires)
                if let original = entry.originalRecordJSON {
                    guard let record = entry.record else { throw MissionError.invalidJournal }
                    try MissionProof.stoppedRecord(record, taskID: entry.taskID, credentials: credentials, context: context, originalJSON: original)
                }
                if restored.state == "sending" { restored.state = "uncertain" }
                return restored
            }
            for entry in parsed.entries { try Self.validate(entry, credentials: credentials, context: context) }
            values = parsed.entries.map { entry in
                var result = entry
                if result.state == .sending { result.state = .uncertain; result.lastError = "The app stopped during delivery. Reconcile before retrying." }
                if result.stopState == "sending" { result.stopState = "uncertain" }
                return result
            }
        } else { values = [] }
    }
    public func entries() -> [MissionEntry] { values }
    public func stopEntries() -> [MissionStopEntry] { stops }
    public func saveDraft(_ draft: MissionDraft) throws {
        guard values.count < 128, !values.contains(where: { $0.id == draft.id }), draft.contextSHA256 == context.digest else { throw MissionError.invalidDraft }
        try context.validate(draft: draft)
        values.append(MissionEntry(draft: draft, state: .draft))
        do { try persist() } catch { values.removeLast(); throw error }
    }
    public func signAndSend(id: String, using client: any MissionTransport) async throws {
        guard let index = values.firstIndex(where: { $0.id == id }), values[index].state == .draft, busy.insert(id).inserted else { throw MissionError.invalidDraft }
        defer { busy.remove(id) }
        let operation = try await client.beginMissionOperation()
        let instruction = try await client.prepareInstruction(draft: values[index].draft, operation: operation)
        var entry = values[index]; entry.instruction = instruction; entry.state = .signed; entry.lastError = nil
        try Self.validate(entry, credentials: credentials, context: context)
        try replace(entry)
        try await deliver(id: id, using: client, operation: operation)
    }
    public func retry(id: String, using client: any MissionTransport) async throws {
        guard let entry = values.first(where: { $0.id == id }), entry.instruction != nil,
              [.signed, .uncertain].contains(entry.state), busy.insert(id).inserted else { throw MissionError.invalidDraft }
        defer { busy.remove(id) }
        let operation = try await client.beginMissionOperation()
        try await deliver(id: id, using: client, operation: operation)
    }
    private func deliver(id: String, using client: any MissionTransport, operation: UInt64) async throws {
        guard var entry = values.first(where: { $0.id == id }), let instruction = entry.instruction else { throw MissionError.invalidJournal }
        entry.state = .sending; entry.lastError = nil; try replace(entry)
        do {
            let record = try await client.submit(instruction: instruction, operation: operation)
            try accept(record, id: id)
        } catch {
            // A failed response is never proof that the assignee did not accept.
            if var current = values.first(where: { $0.id == id }), !current.terminal {
                current.state = .uncertain; current.lastError = error.localizedDescription; try replace(current)
            }
            throw error
        }
    }
    public func reconcile(id: String, using client: any MissionTransport) async throws {
        guard values.contains(where: { $0.id == id && $0.instruction != nil }), busy.insert(id).inserted else { throw MissionError.invalidDraft }
        defer { busy.remove(id) }
        do { try accept(await client.status(taskID: id), id: id) }
        catch {
            if var entry = values.first(where: { $0.id == id }) { entry.lastError = error.localizedDescription; try replace(entry) }
            throw error
        }
    }
    /// Stop has a separate in-flight guard and transport so media, reads or a
    /// pending mission POST cannot prevent an explicit stop attempt.
    public func stop(id: String, using client: any MissionTransport) async throws {
        guard values.contains(where: { $0.id == id && $0.instruction != nil }), stopping.insert(id).inserted else { throw MissionError.invalidDraft }
        defer { stopping.remove(id) }
        let instruction = try await client.prepareStop(taskID: id)
        let stop = try JSONValue.decode(instruction)
        guard let issued = stop["issued_at_unix_ms"]?.int, let expires = stop["expires_at_unix_ms"]?.int else { throw MissionError.invalidJournal }
        try credentials.verifyInstruction(instruction, operation: "stop", payload: GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(id))]), issued: issued, expires: expires)
        guard var entry = values.first(where: { $0.id == id }) else { throw MissionError.invalidJournal }
        entry.stopInstruction = instruction; entry.stopState = "sending"; try replace(entry)
        do {
            let response = try await client.submitStop(instruction: instruction)
            // Direct stop currently returns a Record. A malformed response stays uncertain.
            try accept(response, id: id)
            entry = values.first(where: { $0.id == id })!; entry.stopState = "acknowledged"; try replace(entry)
        } catch {
            entry = values.first(where: { $0.id == id })!; entry.stopState = "uncertain"; entry.lastError = error.localizedDescription; try replace(entry)
            throw error
        }
    }
    public func stopAny(taskID: String, using client: any MissionTransport) async throws {
        guard MissionWire.identifier(taskID), stops.count < 128, stopping.insert(taskID).inserted else { throw MissionError.invalidDraft }
        defer { stopping.remove(taskID) }
        let instruction = try await client.prepareStop(taskID: taskID)
        let decoded = try JSONValue.decode(instruction)
        guard let issued = decoded["issued_at_unix_ms"]?.int, let expires = decoded["expires_at_unix_ms"]?.int else { throw MissionError.invalidJournal }
        try credentials.verifyInstruction(instruction, operation: "stop", payload: GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(taskID))]), issued: issued, expires: expires)
        guard stops.count < 128 else { throw MissionError.journalFull }
        let id = "stop-" + UUID().uuidString.lowercased()
        stops.append(MissionStopEntry(id: id, taskID: taskID, instruction: instruction, state: "sending"))
        do { try persist() } catch { stops.removeLast(); throw error }
        do {
            let response = try await client.submitStopRecord(instruction: instruction)
            let record = response.value
            try MissionProof.stoppedRecord(record, taskID: taskID, credentials: credentials, context: context, originalJSON: response.originalJSON)
            guard let index = stops.firstIndex(where: { $0.id == id }) else { throw MissionError.invalidJournal }
            stops[index].state = "acknowledged"; stops[index].record = record; stops[index].originalRecordJSON = response.originalJSON; try persist()
        } catch {
            if let index = stops.firstIndex(where: { $0.id == id }) { stops[index].state = "uncertain"; stops[index].error = error.localizedDescription; try persist() }
            throw error
        }
    }

    private func accept(_ record: JSONValue, id: String) throws {
        guard var entry = values.first(where: { $0.id == id }), let instruction = entry.instruction else { throw MissionError.invalidJournal }
        let signed = try JSONValue.decode(instruction)
        guard record["proof"] == signed, record["spec"] == signed["payload"],
              let version = record["status"]?["Version"]?.uint, version > 0,
              let phase = record["status"]?["Phase"]?.string,
              ["accepted", "running", "held", "returning", "succeeded", "failed", "canceled", "rejected"].contains(phase) else { throw MissionError.mismatchedStatus }
        if let old = entry.record {
            guard let oldVersion = old["status"]?["Version"]?.uint, version >= oldVersion,
                  !entry.terminal || record == old,
                  version != oldVersion || record == old else { throw MissionError.mismatchedStatus }
        }
        entry.record = record; entry.state = entry.terminal ? .terminal : .delivered; entry.lastError = nil
        try replace(entry)
    }
    private func replace(_ entry: MissionEntry) throws {
        guard let index = values.firstIndex(where: { $0.id == entry.id }) else { throw MissionError.invalidJournal }
        let previous = values[index]; values[index] = entry
        do { try persist() } catch { values[index] = previous; throw error }
    }
    private func persist() throws {
        let bytes = try JSONEncoder().encode(Saved(version: 2, scopeID: scopeID, contextSHA256: context.digest, entries: values, stops: stops))
        try file.save(bytes, replacing: storedDigest); storedDigest = WorldCredentials.digest(bytes)
    }
    private static func validate(_ entry: MissionEntry, credentials: MissionCredentials, context: MissionContext) throws {
        guard MissionWire.identifier(entry.id), entry.draft.contextSHA256 == context.digest else { throw MissionError.invalidJournal }
        try context.validate(draft: entry.draft)
        if let ref = entry.draft.proposal { guard ref.audience == credentials.audience, MissionWire.identifier(ref.id), WorldClient.isDigest(ref.bodySHA256) else { throw MissionError.invalidJournal } }
        guard let bytes = entry.instruction else {
            guard entry.state == .draft, entry.record == nil, entry.stopInstruction == nil else { throw MissionError.invalidJournal }; return
        }
        let instruction = try JSONValue.decode(bytes, limit: 128 << 10)
        guard let issued = instruction["issued_at_unix_ms"]?.int, let expires = instruction["expires_at_unix_ms"]?.int,
              issued >= credentials.notBefore, expires > issued, expires - issued == entry.draft.durationMS,
              let signature = instruction["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.invalidJournal }
        let payload = try context.spec(draft: entry.draft, deadline: expires)
        // CryptoKit may randomize valid Ed25519 signatures. Verify the saved
        // signature over exact canonical bytes; never sign again during restore.
        try credentials.verifyInstruction(bytes, operation: "mission", payload: payload, issued: issued, expires: expires)
        guard signature.count == 64, entry.state != .draft else { throw MissionError.invalidJournal }
        if let record = entry.record {
            guard record["proof"] == instruction, record["spec"] == instruction["payload"],
                  let version = record["status"]?["Version"]?.uint, version > 0,
                  (entry.state == .terminal) == entry.terminal else { throw MissionError.invalidJournal }
        }
        if let stopBytes = entry.stopInstruction {
            let stop = try JSONValue.decode(stopBytes)
            guard let issued = stop["issued_at_unix_ms"]?.int, let expires = stop["expires_at_unix_ms"]?.int else { throw MissionError.invalidJournal }
            try credentials.verifyInstruction(stopBytes, operation: "stop", payload: GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(entry.id))]), issued: issued, expires: expires)
        }
    }
}
