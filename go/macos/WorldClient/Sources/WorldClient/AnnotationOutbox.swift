import Foundation

public actor AnnotationOutbox {
    public nonisolated let scopeID: String
    private let credentials: WorldCredentials
    private let context: AnnotationContext
    private let file: AnnotationFile
    private var state: Stored
    private var storedDigest: String?
    private var syncing = false
    private let monotonic: @Sendable () throws -> AnnotationClockReading
    private struct Stored: Codable {
        var version = 2
        let scopeID: String
        let context: AnnotationContext
        var epoch: UInt64?
        var nextSequence: UInt64 = 1
        var entries: [AnnotationEntry] = []
        var clockAnchor: AnnotationClockAnchor?
        var clockLastNS: Int64?
        var clockFaulted: Bool?
    }

    public init(directory: URL, credentials: WorldCredentials, context: AnnotationContext, monotonic: @escaping @Sendable () throws -> AnnotationClockReading = AnnotationMonotonicClock.read) throws {
        self.monotonic = monotonic
        guard credentials.clockPins == nil || credentials.clockPins?.runID == context.runID else { throw AnnotationError.notAuthorized }
        let scope = try credentials.annotationScopeID(context: context)
        let file = AnnotationFile(directory: directory, scopeID: scope)
        self.scopeID = scope; self.credentials = credentials; self.context = context; self.file = file
        if let data = try file.load() {
            do {
                var saved = try JSONDecoder().decode(Stored.self, from: data)
                guard [1, 2].contains(saved.version), saved.scopeID == scope, saved.context == context,
                      saved.entries.count <= 128, Set(saved.entries.map(\.id)).count == saved.entries.count,
                      saved.nextSequence > 0 else { throw AnnotationError.invalidOutbox }
                for entry in saved.entries {
                    try entry.draft.validate()
                    guard entry.draft.context == context, entry.draft.authorPrincipal == credentials.principal else { throw AnnotationError.invalidOutbox }
                    if let body = entry.signedRecord { try credentials.validateAnnotation(body, draft: entry.draft) }
                    if entry.state == .untimed && (entry.signedRecord != nil || entry.draft.schemaVersion != 2 || entry.draft.clock != nil) { throw AnnotationError.invalidOutbox }
                    if entry.state != .untimed && entry.draft.schemaVersion == 2 && entry.draft.clock == nil { throw AnnotationError.invalidOutbox }
                    if entry.state == .draft && entry.signedRecord != nil { throw AnnotationError.invalidOutbox }
                    if [.pending, .published, .conflict, .denied, .rejected].contains(entry.state) && entry.signedRecord == nil { throw AnnotationError.invalidOutbox }
                    if entry.state == .published && entry.acknowledgedAtUnixMS == nil { throw AnnotationError.invalidOutbox }
                    if entry.state == .pending {
                        guard entry.epoch == saved.epoch, let sequence = entry.sequence, sequence < saved.nextSequence else { throw AnnotationError.invalidOutbox }
                    }
                }
                // An unresolved signed note owns its epoch across restarts. A
                // new session without one obtains a new epoch before signing.
                if !saved.entries.contains(where: { $0.state == .pending }) { saved.epoch = nil; saved.nextSequence = 1 }
                if let anchor = saved.clockAnchor {
                    guard anchor.receivedNS >= 0, anchor.evidence.elapsedNS == 0, !anchor.bootID.isEmpty, anchor.bootID.utf8.count <= 128 else { throw AnnotationError.invalidOutbox }
                    try anchor.evidence.verify(credentials: credentials)
                    if let last = saved.clockLastNS, last < anchor.receivedNS { throw AnnotationError.invalidOutbox }
                    let reading = try monotonic()
                    if reading.bootID == anchor.bootID, reading.nanoseconds < (saved.clockLastNS ?? anchor.receivedNS) { saved.clockFaulted = true }
                    if (try? anchor.at(reading, credentials: credentials)) == nil { saved.clockAnchor = nil; saved.clockLastNS = nil }

                }
                self.state = saved; self.storedDigest = WorldCredentials.digest(data)
            } catch { throw AnnotationError.invalidOutbox }
        } else {
            self.state = Stored(scopeID: scope, context: context)
        }
    }

    public func entries() -> [AnnotationEntry] { state.entries }

    /// This method performs no network operation. The immutable draft reaches
    /// disk before it is returned to the UI or a sync can begin.
    @discardableResult
    func saveLegacyDraft(text: String, positionMM: [Int64], lifetimeMS: Int64 = 86_400_000, now: Date = Date()) throws -> AnnotationEntry {
        guard state.entries.count < 128 else { throw AnnotationError.outboxFull }
        guard now.timeIntervalSince1970.isFinite, now.timeIntervalSince1970 > 0, now.timeIntervalSince1970 < 253_402_214_400 else { throw AnnotationError.invalidNote }
        let authored = Int64(now.timeIntervalSince1970 * 1000)
        guard lifetimeMS > 0, lifetimeMS <= 86_400_000,
              authored >= credentials.notBefore, authored < credentials.notAfter else { throw AnnotationError.notAuthorized }
        let draft = try AnnotationDraft(authorPrincipal: credentials.principal, text: text, positionMM: positionMM, authoredAtUnixMS: authored, expiresAtUnixMS: authored + lifetimeMS, context: context)
        let entry = AnnotationEntry(draft: draft, state: .draft)
        var next = state; next.entries.append(entry)
        try persist(next)
        return entry
    }

    @discardableResult
    public func refreshClock(using client: WorldClient) async throws -> AnnotationClockEvidence {
        guard try await client.annotationScopeID(context: context) == scopeID else { throw AnnotationError.notAuthorized }
        let anchor = try await client.bootstrapAnnotationClock()
        let evidence = try anchor.at(monotonic(), credentials: credentials)
        try saveClockAnchor(anchor)
        return evidence
    }
    func saveClockAnchor(_ anchor: AnnotationClockAnchor) throws {
        let reading = try monotonic()
        _ = try anchor.at(reading, credentials: credentials)
        var next = state; next.clockAnchor = anchor; next.clockLastNS = reading.nanoseconds; next.clockFaulted = false; try persist(next)
    }
    @discardableResult public func prepareClock(using client: WorldClient) async throws -> AnnotationClockEvidence {
        if let anchor = state.clockAnchor, let evidence = try? clockEvidence() {
            try await client.restoreClock(anchor); return evidence
        }
        return try await refreshClock(using: client)
    }
    public func clockEvidence() throws -> AnnotationClockEvidence {
        guard let anchor = state.clockAnchor, state.clockFaulted != true else { throw AnnotationClockError.unavailable }
        let reading = try monotonic()
        guard reading.bootID == anchor.bootID, reading.nanoseconds >= (state.clockLastNS ?? anchor.receivedNS) else {
            state.clockFaulted = true; throw AnnotationClockError.unavailable
        }
        let evidence = try anchor.at(reading, credentials: credentials)
        state.clockLastNS = reading.nanoseconds
        return evidence
    }
    @discardableResult public func saveDraft(text: String, positionMM: [Int64], lifetimeMS: Int64 = 86_400_000) throws -> AnnotationEntry {
        guard state.entries.count < 128 else { throw AnnotationError.outboxFull }
        let evidence = try? clockEvidence()
        if let evidence {
            let interval = try evidence.interval()
            guard interval.lowerUnixMS >= credentials.notBefore, interval.upperUnixMS < credentials.notAfter,
                  credentials.clockPins?.runID == context.runID else { throw AnnotationError.notAuthorized }
        }
        let draft = try AnnotationDraft(authorPrincipal: credentials.principal, text: text, positionMM: positionMM, lifetimeMS: lifetimeMS, context: context, clock: evidence)
        let entry = AnnotationEntry(draft: draft, state: evidence == nil ? .untimed : .draft,
            issue: evidence == nil ? "Untimed text. Reconnecting will not assign an authoring time. Create a separate timed note when time is known." : nil)
        var next = state; next.entries.append(entry); try persist(next); return entry
    }
    public func sync(using client: WorldClient) async throws {
        let evidence = try await refreshClock(using: client)
        let interval = try evidence.interval()
        try await drain(using: client, nowMS: interval.lowerUnixMS, latestMS: interval.upperUnixMS)
    }
    func sync(using client: WorldClient, now: Date) async throws {
        let value = Int64(now.timeIntervalSince1970 * 1000)
        try await drain(using: client, nowMS: value, latestMS: value)
    }

    /// Only exact persisted bodies are retried. Conflicts and expired notes are
    /// retained for inspection; this never rewrites their epoch or lifetime.
    private func drain(using client: WorldClient, nowMS: Int64, latestMS: Int64) async throws {
        guard !syncing else { return }
        syncing = true
        defer { syncing = false }
        guard try await client.annotationScopeID(context: context) == scopeID else { throw AnnotationError.notAuthorized }
        var next = state
        for index in next.entries.indices where [.draft, .pending].contains(next.entries[index].state) {
            if next.entries[index].draft.expiresAtUnixMS <= nowMS {
                next.entries[index].state = .expired
                next.entries[index].issue = "The authored lifetime ended. This note will not be renewed."
            }
        }
        if !next.entries.contains(where: { $0.state == .pending }) {
            next.epoch = nil; next.nextSequence = 1
        }
        try persist(next)
        let ids = state.entries.filter { [.draft, .pending].contains($0.state) }.map(\.id)
        guard !ids.isEmpty else { return }
        if state.epoch == nil {
            do {
                let epoch = try await client.annotationEpoch()
                next = state; next.epoch = epoch; next.nextSequence = 1
                try persist(next)
            } catch {
                try recordIssue(ids: ids, issue: "Waiting for an annotation epoch. Check connection and contributor credentials.")
                throw error
            }
        }
        var fencedEpoch = false
        for id in ids {
            try Task.checkCancellation()
            guard let index = state.entries.firstIndex(where: { $0.id == id }), [.draft, .pending].contains(state.entries[index].state) else { continue }
            if latestMS >= state.entries[index].draft.expiresAtUnixMS { continue }
            if state.entries[index].state == .draft {
                if fencedEpoch { continue }
                guard let epoch = state.epoch, state.nextSequence < UInt64.max else { throw AnnotationError.invalidOutbox }
                let body = try credentials.signAnnotation(state.entries[index].draft, epoch: epoch, sequence: state.nextSequence)
                next = state; next.entries[index].signedRecord = body; next.entries[index].state = .pending
                next.entries[index].issue = nil; next.nextSequence += 1
                try persist(next)
            }
            guard let body = state.entries[index].signedRecord else { throw AnnotationError.invalidOutbox }
            do {
                try await client.publishAnnotation(body, draft: state.entries[index].draft)
                next = state; next.entries[index].state = .published
                next.entries[index].acknowledgedAtUnixMS = nowMS
                next.entries[index].issue = nil
                try persist(next)
            } catch {
                next = state
                let status: Int? = if case WorldClientError.httpStatus(let code) = error { code } else { nil }
                switch status {
                case 409:
                    fencedEpoch = true
                    next.entries[index].state = .conflict; next.entries[index].issue = "Gateway epoch or immutable note conflict. Original bytes retained; no automatic rebase."
                case 410: next.entries[index].state = .expired; next.entries[index].issue = "Gateway rejected an expired note. Its lifetime is unchanged."
                case 401, 403: next.entries[index].state = .denied; next.entries[index].issue = "Gateway denied this contribution. Original signed note retained for review."
                case 400: next.entries[index].state = .rejected; next.entries[index].issue = "Gateway rejected the note format. Original signed note retained."
                default: next.entries[index].issue = "Not acknowledged. Exact signed bytes remain pending for retry."
                }
                try persist(next)
                if status == nil || (status ?? 500) >= 500 { throw error }
            }
        }
        // Resolved notes retain their exact historical bodies. A later new
        // draft may acquire a fresh epoch after all pending records resolve.
        if !state.entries.contains(where: { $0.state == .pending }) {
            next = state; next.epoch = nil; next.nextSequence = 1
            try persist(next)
        }
    }

    private func recordIssue(ids: [String], issue: String) throws {
        var next = state
        for i in next.entries.indices where ids.contains(next.entries[i].id) { next.entries[i].issue = issue }
        try persist(next)
    }
    private func persist(_ candidate: Stored) throws {
        var next = candidate; next.version = 2
        let bytes = try JSONEncoder().encode(next)
        try file.save(bytes, replacing: storedDigest)
        state = next; storedDigest = WorldCredentials.digest(bytes)
    }
}
