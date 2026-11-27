import CryptoKit
import Foundation
import XCTest
@testable import WorldClient

@MainActor
final class AnnotationTests: XCTestCase {
    func fixture() throws -> JSONValue {
        let url = Bundle.module.url(forResource: "native-annotation", withExtension: "json", subdirectory: "Fixtures")!
        return try JSONValue.decode(Data(contentsOf: url))
    }
    func credentials(url: URL? = nil) throws -> WorldCredentials {
        var config = try fixture()["credentials"]!.object!
        if let url {
            config["url"] = .string(url.absoluteString)
            var signed = config["grant"]!.object!, grant = signed["grant"]!.object!
            grant["not_before_unix_ms"] = .integer(Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000))
            grant["not_after_unix_ms"] = .integer(Int64(Date().addingTimeInterval(86_400).timeIntervalSince1970 * 1000))
            signed["grant"] = .object(grant); config["grant"] = .object(signed)
        }
        return try WorldCredentials(data: JSONValue.object(config).encoded())
    }
    func context() throws -> AnnotationContext { try AnnotationContext(runID: "office-run", frameID: "site", frameRevision: "office-v1") }
    func directory() -> URL { FileManager.default.temporaryDirectory.appendingPathComponent("annotation-test-" + UUID().uuidString) }

    func testGoRecordAndRequestSigningFixture() throws {
        let vector = try fixture(), note = vector["note"]!
        let credentials = try credentials()
        let draft = try AnnotationDraft(id: note["id"]!.string!, authorPrincipal: note["author_principal"]!.string!, text: note["text"]!.string!, positionMM: note["position_mm"]!.array!.map { $0.int! }, authoredAtUnixMS: note["authored_at_unix_ms"]!.int!, expiresAtUnixMS: note["expires_at_unix_ms"]!.int!, context: context())
        let body = try credentials.signAnnotation(draft, epoch: vector["epoch"]!.uint!, sequence: vector["sequence"]!.uint!)
        let actual = try JSONValue.decode(body)
        let expected = vector["expected_signed_record"]!
        XCTAssertEqual(actual["component"], vector["expected_component"])
        XCTAssertEqual(String(data: try credentials.annotationComponent(draft, epoch: 7, sequence: 1), encoding: .utf8), vector["expected_component_json"]!.string)
        let bytes = Data(base64Encoded: vector["expected_record_signing_bytes_base64"]!.string!)!
        XCTAssertTrue(credentials.key.publicKey.isValidSignature(Data(base64Encoded: actual["signature"]!.string!)!, for: bytes))
        XCTAssertTrue(credentials.key.publicKey.isValidSignature(Data(base64Encoded: expected["signature"]!.string!)!, for: bytes))
        try credentials.validateAnnotation(body, draft: draft)
        let expectedBody = Data(vector["expected_signed_record_json"]!.string!.utf8)
        let request = try credentials.signedAuthorization(operation: "annotation-write", body: expectedBody, nonce: vector["nonce"]!.string!, issuedAtUnixMS: vector["request_issued_at_unix_ms"]!.int!, lifetimeMS: vector["request_lifetime_ms"]!.int!)
        let signature = try JSONValue.decode(request)["signature"]!.string!
        XCTAssertTrue(credentials.key.publicKey.isValidSignature(Data(base64Encoded: signature)!, for: Data(base64Encoded: vector["expected_request_signing_bytes_base64"]!.string!)!))
    }

    func testDraftPersistsOfflineAndIsIsolatedByEndpointAndRun() async throws {
        let server = try LoopbackServer { _ in .init(status: 503) }
        defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), context = try context()
        let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        let note = try await outbox.saveLegacyDraft(text: "Inspect this area", positionMM: [1, 2, 3])
        XCTAssertTrue(server.requests.isEmpty)
        let reopened = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        let saved = await reopened.entries()
        XCTAssertEqual(saved, [note]); XCTAssertNil(note.signedRecord)
        let otherContext = try AnnotationContext(runID: "different-run", frameID: "site", frameRevision: "office-v1")
        let other = try AnnotationOutbox(directory: dir, credentials: credentials, context: otherContext)
        let otherEntries = await other.entries(); XCTAssertTrue(otherEntries.isEmpty)
        XCTAssertNotEqual(outbox.scopeID, other.scopeID)
        let changed = try self.credentials(url: URL(string: "http://127.0.0.1:12345")!)
        XCTAssertNotEqual(try changed.annotationScopeID(context: context), outbox.scopeID)
    }

    func testExactSignedBodySurvivesFailureAndRestartThenPublishesOnce() async throws {
        let behavior = Responses()
        let server = try LoopbackServer { request in behavior.respond(request) }
        defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), context = try context(), client = WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
        let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        let draft = try await outbox.saveLegacyDraft(text: "Check door", positionMM: [1000, 0, 2500])
        do { try await outbox.sync(using: client, now: Date()); XCTFail("expected unavailable") } catch { }
        let pending = await outbox.entries()
        XCTAssertEqual(pending.first?.state, .pending)
        let original = try XCTUnwrap(pending.first?.signedRecord)
        behavior.set(status: 200)
        let restarted = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        try await restarted.sync(using: client, now: Date())
        try await restarted.sync(using: client, now: Date())
        let published = await restarted.entries()
        XCTAssertEqual(published.first?.state, .published)
        XCTAssertEqual(published.first?.signedRecord, original)
        XCTAssertEqual(published.first?.draft, draft.draft)
        XCTAssertEqual(server.requests.filter { $0.path == "/v1/annotations/epoch" }.count, 1)
        let sends = server.requests.filter { $0.path == "/v1/annotations" }
        XCTAssertEqual(sends.count, 2); XCTAssertEqual(sends[0].body, sends[1].body)
        let nonces = try sends.map { try JSONValue.decode(Data(base64Encoded: $0.headers["x-world-authorization"]!)!)["nonce"]!.string! }
        XCTAssertNotEqual(nonces[0], nonces[1])
    }

    func testConflictDoesNotRebaseAndWrongAcknowledgementNeverPublishes() async throws {
        for status in [409, 401, 400, 410, 201] {
            let behavior = Responses(); behavior.set(status: status)
            let server = try LoopbackServer { behavior.respond($0) }; defer { server.stop() }
            let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
            let credentials = try credentials(url: server.url), client = WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
            let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context())
            _ = try await outbox.saveLegacyDraft(text: "Immutable", positionMM: [0, 0, 0])
            try? await outbox.sync(using: client, now: Date())
            let first = await outbox.entries()
            XCTAssertNotEqual(first[0].state, .published)
            try? await outbox.sync(using: client, now: Date())
            let second = await outbox.entries()
            XCTAssertEqual(first[0].signedRecord, second[0].signedRecord)
            XCTAssertEqual(server.requests.filter { $0.path == "/v1/annotations/epoch" }.count, 1)
            if status != 201 { XCTAssertEqual(server.requests.filter { $0.path == "/v1/annotations" }.count, 1) }
        }
    }

    func testExpiryCorruptionAndConcurrentWriterFailClosed() async throws {
        let server = try LoopbackServer { _ in .init(status: 503) }; defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), context = try context()
        let first = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        let second = try AnnotationOutbox(directory: dir, credentials: credentials, context: context)
        let created = try await first.saveLegacyDraft(text: "Short note", positionMM: [0, 0, 0], lifetimeMS: 1)
        do { _ = try await second.saveLegacyDraft(text: "Racing note", positionMM: [0, 0, 0]); XCTFail("concurrent writer replaced draft") } catch { }
        try await first.sync(using: WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) }), now: Date(timeIntervalSince1970: Double(created.draft.expiresAtUnixMS + 1) / 1000))
        let entries = await first.entries(); XCTAssertEqual(entries[0].state, .expired); XCTAssertTrue(server.requests.isEmpty)
        let path = dir.appendingPathComponent(first.scopeID + ".json")
        try Data("{}".utf8).write(to: path)
        XCTAssertThrowsError(try AnnotationOutbox(directory: dir, credentials: credentials, context: context))
    }

    func testReadersCannotAnnotateAndDraftBoundsAreStrict() throws {
        let reader = try WorldCredentials(data: ProtocolTests.vectors()[0]["credentials"]!.encoded())
        XCTAssertFalse(reader.canAnnotate)
        XCTAssertThrowsError(try reader.annotationScopeID(context: context()))
        let at: Int64 = 1794312000000
        for text in ["   \n", String(repeating: "é", count: 1025)] {
            XCTAssertThrowsError(try AnnotationDraft(authorPrincipal: "operator", text: text, positionMM: [0, 0, 0], authoredAtUnixMS: at, expiresAtUnixMS: at + 1000, context: context()))
        }
        XCTAssertThrowsError(try AnnotationDraft(authorPrincipal: "operator", text: "Note", positionMM: [1_000_000_001, 0, 0], authoredAtUnixMS: at, expiresAtUnixMS: at + 1000, context: context()))
        XCTAssertThrowsError(try AnnotationDraft(authorPrincipal: "operator", text: "Note", positionMM: [0, 0, 0], authoredAtUnixMS: at, expiresAtUnixMS: at + 86_400_001, context: context()))
    }

    func testNewDraftAfterConflictAcquiresNewEpochWithoutRewritingOldBody() async throws {
        let behavior = Responses(); behavior.set(status: 409)
        let server = try LoopbackServer { behavior.respond($0) }; defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), client = WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
        let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context())
        _ = try await outbox.saveLegacyDraft(text: "Conflicted note", positionMM: [0, 0, 0])
        try await outbox.sync(using: client, now: Date())
        let before = await outbox.entries()
        XCTAssertEqual(before[0].state, .conflict)
        behavior.set(status: 200)
        _ = try await outbox.saveLegacyDraft(text: "A separate new assertion", positionMM: [1, 0, 0])
        try await outbox.sync(using: client, now: Date())
        let after = await outbox.entries()
        XCTAssertEqual(after[0].state, .conflict)
        XCTAssertEqual(after[0].signedRecord, before[0].signedRecord)
        XCTAssertEqual(after[1].state, .published)
        XCTAssertEqual(after[0].epoch, 7); XCTAssertEqual(after[1].epoch, 8)
        XCTAssertEqual(server.requests.filter { $0.path == "/v1/annotations/epoch" }.count, 2)
    }

    func testFencedPendingRecordDoesNotConsumeAlreadyQueuedUnsignedDraft() async throws {
        let behavior = Responses()
        let server = try LoopbackServer { behavior.respond($0) }; defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), client = WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
        let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context())
        _ = try await outbox.saveLegacyDraft(text: "Previously signed", positionMM: [0, 0, 0])
        try? await outbox.sync(using: client, now: Date())
        let pending = await outbox.entries()
        _ = try await outbox.saveLegacyDraft(text: "Still unsigned", positionMM: [1, 0, 0])
        behavior.set(status: 409)
        try await outbox.sync(using: client, now: Date())
        let fenced = await outbox.entries()
        XCTAssertEqual(fenced[0].state, .conflict)
        XCTAssertEqual(fenced[0].signedRecord, pending[0].signedRecord)
        XCTAssertEqual(fenced[1].state, .draft); XCTAssertNil(fenced[1].signedRecord)
        behavior.set(status: 200)
        try await outbox.sync(using: client, now: Date())
        let final = await outbox.entries()
        XCTAssertEqual(final[0].state, .conflict); XCTAssertEqual(final[0].signedRecord, pending[0].signedRecord)
        XCTAssertEqual(final[1].state, .published); XCTAssertEqual(final[1].epoch, 8)
    }

    func testExpiredPendingEpochIsReleasedForNewDraft() async throws {
        let behavior = Responses()
        let server = try LoopbackServer { behavior.respond($0) }; defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let credentials = try credentials(url: server.url), client = WorldClient(credentials: credentials, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
        let outbox = try AnnotationOutbox(directory: dir, credentials: credentials, context: context())
        let old = try await outbox.saveLegacyDraft(text: "Expires before recovery", positionMM: [0, 0, 0], lifetimeMS: 1000)
        try? await outbox.sync(using: client, now: Date())
        let pending = await outbox.entries()
        try await outbox.sync(using: client, now: Date(timeIntervalSince1970: Double(old.draft.expiresAtUnixMS + 1) / 1000))
        behavior.set(status: 200)
        _ = try await outbox.saveLegacyDraft(text: "Separately authored", positionMM: [1, 0, 0])
        try await outbox.sync(using: client, now: Date())
        let entries = await outbox.entries()
        XCTAssertEqual(entries[0].state, .expired); XCTAssertEqual(entries[0].signedRecord, pending[0].signedRecord)
        XCTAssertEqual(entries[1].state, .published); XCTAssertEqual(entries[1].epoch, 8)
    }
}

final class Responses: @unchecked Sendable {
    private let lock = NSLock()
    private var status = 503
    private var nextEpoch = 7
    func set(status: Int) { lock.lock(); self.status = status; lock.unlock() }
    func respond(_ request: LoopbackServer.Request) -> LoopbackServer.Response {
        if request.path == "/v1/annotations/epoch" {
            lock.lock(); let epoch = nextEpoch; nextEpoch += 1; lock.unlock()
            return .init(body: Data("{\"epoch\":\(epoch)}".utf8))
        }
        lock.lock(); let status = self.status; lock.unlock()
        guard status == 200 || status == 201, let record = try? JSONValue.decode(request.body), let component = record["component"] else { return .init(status: status) }
        let result: JSONValue = .object(["result": .string("applied"), "component": status == 201 ? .object([:]) : component])
        return .init(body: (try? result.encoded()) ?? Data())
    }
}
