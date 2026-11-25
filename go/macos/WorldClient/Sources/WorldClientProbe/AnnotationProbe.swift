import Foundation
import WorldClient

enum AnnotationProbe {
    struct Entry: Encodable {
        let id: String
        let text: String
        let authored_at_unix_ms: Int64
        let expires_at_unix_ms: Int64
        let clock: AnnotationClockEvidence?
        let interval: AnnotationTimeInterval?
        let schema_version: Int
        let state: String
        let body_sha256: String?
        let epoch: UInt64?
        let sequence: UInt64?
    }
    struct Report: Encodable {
        let mode: String
        let scopeID: String
        let pending: Int
        let published: Int
        let conflicts: Int
        let entries: [Entry]
        let notes: [JSONValue]
        let clock: AnnotationClockEvidence?
        let interval: AnnotationTimeInterval?
        let boot_continuity: String
        let error: String?
    }
    static func run(mode: String, profile: JSONValue, credentials: WorldCredentials, directory: URL) async throws {
        guard let raw = profile["annotation_context"] else { throw AnnotationError.notAuthorized }
        let context = try AnnotationContext(json: raw)
        let outbox = try AnnotationOutbox(directory: directory.appendingPathComponent("Annotations", isDirectory: true), credentials: credentials, context: context)
        var notes: [JSONValue] = []
        var failure: String?
        if mode == "annotation-clock" {
            try await outbox.refreshClock(using: WorldClient(credentials: credentials))
        } else if mode == "annotation-draft" || mode == "annotation-untimed" {
            if mode == "annotation-untimed", (try? await outbox.clockEvidence()) != nil { throw AnnotationError.invalidNote }
            try await outbox.saveDraft(text: "Native offline annotation", positionMM: [1250, 0, 2500])
        } else if mode == "annotation-sync" {
            let client = WorldClient(credentials: credentials)
            do { try await outbox.sync(using: client) } catch { failure = error.localizedDescription }
            if let snapshot = try? await client.query() { notes = snapshot.components.filter { ["wendy.annotation.note.v1", "wendy.annotation.note.v2"].contains($0.type) }.map(\.raw) }
        }
        let entries = await outbox.entries()
        let clock = try? await outbox.clockEvidence()
        let boot = try AnnotationMonotonicClock.read().bootID
        try WorldClientProbe.output(Report(mode: mode, scopeID: outbox.scopeID,
            pending: entries.filter { [.draft, .pending].contains($0.state) }.count,
            published: entries.filter { $0.state == .published }.count,
            conflicts: entries.filter { [.conflict, .denied, .rejected].contains($0.state) }.count,
            entries: entries.map { Entry(id: $0.id, text: $0.draft.text, authored_at_unix_ms: $0.draft.authoredAtUnixMS, expires_at_unix_ms: $0.draft.expiresAtUnixMS, clock: $0.draft.clock, interval: try? $0.draft.clock?.interval(), schema_version: $0.draft.schemaVersion ?? 1, state: $0.state.rawValue, body_sha256: $0.bodySHA256, epoch: $0.epoch, sequence: $0.sequence) },
            notes: notes, clock: clock, interval: try? clock?.interval(), boot_continuity: boot.hasPrefix("boot:") ? "kernel-boot-session" : "process-only", error: failure))
    }
}
