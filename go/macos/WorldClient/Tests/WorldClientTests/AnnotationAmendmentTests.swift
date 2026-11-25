import Foundation
import XCTest
@testable import WorldClient

@MainActor final class AnnotationAmendmentTests: XCTestCase {
    func fixture() throws -> (WorldCredentials, AnnotationEntry, AnnotationClockEvidence) {
        let raw = try JSONValue.decode(Data(contentsOf: Bundle.module.url(forResource: "native-annotation-clock", withExtension: "json", subdirectory: "Fixtures")!))
        let credentials = try WorldCredentials(data: raw["credentials"]!.encoded())
        let clock = try JSONDecoder().decode(AnnotationClockEvidence.self, from: raw["evidence"]!.encoded())
        let n = raw["note"]!, frame = raw["frame"]!
        let context = try AnnotationContext(runID: raw["run_id"]!.string!, frameID: frame["id"]!.string!, frameRevision: frame["revision"]!.string!)
        let draft = try AnnotationDraft(id: n["id"]!.string!, authorPrincipal: n["author_principal"]!.string!, text: n["text"]!.string!, positionMM: n["position_mm"]!.array!.map {$0.int!}, lifetimeMS: n["expires_at_unix_ms"]!.int! - n["authored_at_unix_ms"]!.int!, context: context, clock: clock)
        let body = try credentials.signAnnotation(draft, epoch: 7, sequence: 1)
        let original = AnnotationEntry(draft: draft, state: .published, signedRecord: body, acknowledgedAtUnixMS: draft.authoredAtUnixMS)
        let later = AnnotationClockEvidence(request: clock.request, observation: clock.observation, rttNS: clock.rttNS, elapsedNS: clock.elapsedNS + 100_000_000)
        return (credentials, original, later)
    }
    func testCanonicalSignedCorrectionAncestryAndTerminalWithdrawal() throws {
        let (credentials, original, clock) = try fixture()
        let correction = AnnotationAmendmentDraft(original: original, predecessor: nil, action: "correct", text: "East entrance 東京 <>&", positionMM: [2000,0,1000], clock: clock)
        let body = try credentials.signAmendment(correction, epoch: 8)
        let parsed = try credentials.decodeAmendment(body, original: original)
        XCTAssertEqual(parsed.draft, correction)
        let withdraw = AnnotationAmendmentDraft(original: original, predecessor: body, action: "withdraw", text: "", positionMM: original.draft.positionMM, clock: clock)
        let withdrawn = try credentials.signAmendment(withdraw, epoch: 9)
        XCTAssertEqual(try credentials.decodeAmendment(withdrawn, original: original).draft, withdraw)
        let after = AnnotationAmendmentDraft(original: original, predecessor: withdrawn, action: "correct", text: "Restore", positionMM: [0,0,0], clock: clock)
        XCTAssertThrowsError(try credentials.signAmendment(after, epoch: 10))
        let payload = try JSONValue.decode(withdrawn)["component"]!["payload"]!
        XCTAssertEqual(payload["expires_at_unix_ms"]?.int, original.draft.expiresAtUnixMS)
        XCTAssertEqual(payload["original_sha256"]?.string, original.bodySHA256)
        XCTAssertEqual(payload["predecessor_sha256"]?.string, parsed.amendmentSHA256)
        var changed = body; changed[changed.count-5] ^= 1
        XCTAssertThrowsError(try credentials.decodeAmendment(changed, original: original))
    }
    func testExactPendingBodyReopensWithoutNetworkOrNewEpoch() async throws {
        let (credentials, original, clock) = try fixture()
        let draft = AnnotationAmendmentDraft(original: original, predecessor: nil, action: "withdraw", text: "", positionMM: original.draft.positionMM, clock: clock)
        let body = try credentials.signAmendment(draft, epoch: 8)
        let entry = AnnotationAmendmentEntry(draft: draft, signedRecord: body, state: .pending)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let scope = WorldCredentials.digest(Data((try credentials.annotationScopeID(context: original.draft.context) + ":amendments:v1").utf8))
        let file = AnnotationFile(directory: directory, scopeID: scope)
        let raw = try JSONEncoder().encode([entry]); try file.save(raw, replacing: nil)
        let reopened = try AnnotationAmendmentOutbox(directory: directory, credentials: credentials, context: original.draft.context)
        let entries = await reopened.snapshot(); XCTAssertEqual(entries, [entry]); XCTAssertEqual(entries[0].signedRecord, body)
        XCTAssertThrowsError(try file.save(Data("[]".utf8), replacing: nil))
        XCTAssertEqual(try file.load(), raw)
    }
    func testDisplayDeadlineAndRebootCannotRestoreCurrentText() {
        let row: JSONValue = .object(["state":.string("corrected"),"text":.string("Corrected"),"display_boot_id":.string("boot:a"),"display_until_ns":.integer(200)])
        XCTAssertEqual(AnnotationViewPresentation.state(row, offline: false, reading: .init(bootID: "boot:a", nanoseconds: 100)), "corrected")
        XCTAssertEqual(AnnotationViewPresentation.state(row, offline: false, reading: .init(bootID: "boot:a", nanoseconds: 200)), "historical_unavailable")
        XCTAssertEqual(AnnotationViewPresentation.state(row, offline: false, reading: .init(bootID: "boot:b", nanoseconds: 0)), "historical_unavailable")
        XCTAssertEqual(AnnotationViewPresentation.state(row, offline: true, reading: .init(bootID: "boot:a", nanoseconds: 100)), "historical_unavailable")
        let withdrawn: JSONValue = .object(["state":.string("withdrawn")])
        XCTAssertEqual(AnnotationViewPresentation.state(withdrawn, offline: true), "historical_withdrawn")
    }
    func testExpiredClockAndDifferentOriginalRejectBeforeSigning() throws {
        let (credentials, original, clock) = try fixture()
        let late = AnnotationClockEvidence(request: clock.request, observation: clock.observation, rttNS: clock.rttNS, elapsedNS: 3_600_000_000_001)
        let draft = AnnotationAmendmentDraft(original: original, predecessor: nil, action: "correct", text: "Late", positionMM: [0,0,0], clock: late)
        XCTAssertThrowsError(try credentials.signAmendment(draft, epoch: 8))
        let valid = AnnotationAmendmentDraft(original: original, predecessor: nil, action: "correct", text: "First", positionMM: [0,0,0], clock: clock)
        let body = try credentials.signAmendment(valid, epoch: 8)
        var changed = try JSONValue.decode(body).object!, component = changed["component"]!.object!, payload = component["payload"]!.object!
        payload["original_sha256"] = .string(String(repeating: "0", count: 64)); component["payload"] = .object(payload); changed["component"] = .object(component)
        XCTAssertThrowsError(try credentials.decodeAmendment(JSONValue.object(changed).encoded(), original: original))
    }
}
