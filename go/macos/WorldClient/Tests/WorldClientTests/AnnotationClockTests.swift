import CryptoKit
import Foundation
import XCTest
@testable import WorldClient

private final class TestMonotonic: @unchecked Sendable {
    private let lock = NSLock()
    private var value = AnnotationClockReading(bootID: "boot:test-a", nanoseconds: 10_000_000_000)
    func read() -> AnnotationClockReading { lock.lock(); defer { lock.unlock() }; return value }
    func set(_ ns: Int64, boot: String = "boot:test-a") { lock.lock(); defer { lock.unlock() }; value = AnnotationClockReading(bootID: boot, nanoseconds: ns) }
}

@MainActor final class AnnotationClockTests: XCTestCase {
    func vector() throws -> JSONValue {
        try JSONValue.decode(Data(contentsOf: Bundle.module.url(forResource: "native-annotation-clock", withExtension: "json", subdirectory: "Fixtures")!))
    }
    func credentials(_ url: URL? = nil) throws -> WorldCredentials {
        var value = try vector()["credentials"]!.object!
        if let url { value["url"] = .string(url.absoluteString) }
        return try WorldCredentials(data: JSONValue.object(value).encoded())
    }
    func evidence() throws -> AnnotationClockEvidence { try JSONDecoder().decode(AnnotationClockEvidence.self, from: vector()["evidence"]!.encoded()) }
    func anchor() throws -> AnnotationClockAnchor {
        let old = try evidence()
        return AnnotationClockAnchor(evidence: AnnotationClockEvidence(request: old.request, observation: old.observation, rttNS: old.rttNS, elapsedNS: 0), bootID: "boot:test-a", receivedNS: 10_000_000_000)
    }
    func context() throws -> AnnotationContext {
        let v = try vector()
        return try AnnotationContext(runID: v["run_id"]!.string!, frameID: v["frame"]!["id"]!.string!, frameRevision: v["frame"]!["revision"]!.string!)
    }
    func directory() -> URL { FileManager.default.temporaryDirectory.appendingPathComponent("clock-outbox-" + UUID().uuidString) }

    func testExactGoProofIntervalAndV2SignedRecord() throws {
        let v = try vector(), c = try credentials(), e = try evidence()
        try e.verify(credentials: c)
        let interval = try e.interval()
        XCTAssertEqual(interval.lowerUnixMS, v["interval"]?["lower_unix_ms"]?.int)
        XCTAssertEqual(interval.upperUnixMS, v["interval"]?["upper_unix_ms"]?.int)
        XCTAssertEqual(try ClockWire.signed(e.request, message: "request", keys: ClockWire.requestKeys), Data(v["request_json"]!.string!.utf8))
        XCTAssertEqual(try ClockWire.signed(e.observation, message: "observation", keys: ClockWire.observationKeys), Data(v["observation_json"]!.string!.utf8))
        let note = v["note"]!
        let draft = try AnnotationDraft(id: note["id"]!.string!, authorPrincipal: c.principal, text: note["text"]!.string!, positionMM: note["position_mm"]!.array!.map { $0.int! }, lifetimeMS: note["expires_at_unix_ms"]!.int! - note["authored_at_unix_ms"]!.int!, context: context(), clock: e)
        let body = try c.signAnnotation(draft, epoch: v["epoch"]!.uint!, sequence: v["sequence"]!.uint!)
        let expected = Data(v["expected_signed_record_json"]!.string!.utf8)
        let actualBytes = c.annotationSigningBytes(component: try c.annotationComponent(draft, epoch: v["epoch"]!.uint!, sequence: v["sequence"]!.uint!), grant: c.canonicalGrant, authored: draft.authoredAtUnixMS, expiry: draft.authoredAtUnixMS + 86_400_000)
        let expectedBytes = Data(base64Encoded: v["expected_record_signing_bytes_base64"]!.string!)!
        XCTAssertEqual(actualBytes, expectedBytes)
        let goSignature = Data(base64Encoded: v["expected_signed_record"]!["signature"]!.string!)!
        XCTAssertTrue(c.key.publicKey.isValidSignature(goSignature, for: actualBytes))
        XCTAssertTrue(c.key.publicKey.isValidSignature(try JSONValue.decode(body)["signature"]!.string.flatMap { Data(base64Encoded: $0) }!, for: expectedBytes))
        XCTAssertEqual(try JSONValue.decode(body)["component"], try JSONValue.decode(expected)["component"])
        try c.validateAnnotation(body, draft: draft)
    }

    func testTamperedProofAndWrongPinsNeverEstablishTime() throws {
        let c = try credentials(), original = try evidence()
        for field in ["nonce", "run_id", "clock_domain", "gateway_principal", "operator_principal", "request_sha256"] {
            var outer = original.observation.object!, value = outer["observation"]!.object!
            value[field] = .string("wrong"); outer["observation"] = .object(value)
            let changed = AnnotationClockEvidence(request: original.request, observation: .object(outer), rttNS: original.rttNS, elapsedNS: original.elapsedNS)
            XCTAssertThrowsError(try changed.verify(credentials: c), field)
        }
        for field in ["root_public_key", "gateway_key_sha256", "gateway_principal", "run_id"] {
            var config = try vector()["credentials"]!.object!, pins = config["clock_trust"]!.object!
            pins[field] = .string(field == "root_public_key" ? Data(repeating: 7, count: 32).base64EncodedString() : field == "gateway_key_sha256" ? String(repeating: "0", count: 64) : "wrong")
            if field == "run_id" { pins["clock_domain"] = .string("simulation:wrong") }
            config["clock_trust"] = .object(pins)
            let wrong = try WorldCredentials(data: JSONValue.object(config).encoded())
            XCTAssertThrowsError(try original.verify(credentials: wrong), field)
        }
    }

    func testHoldoverIncludesSuspendAndRejectsBootRollbackAndMalformedAnchor() throws {
        let c = try credentials(), a = try anchor()
        let before = try a.at(.init(bootID: a.bootID, nanoseconds: a.receivedNS), credentials: c).interval()
        let after = try a.at(.init(bootID: a.bootID, nanoseconds: a.receivedNS + 3_600_000_000_000), credentials: c).interval()
        XCTAssertGreaterThan(after.upperUnixMS - after.lowerUnixMS, before.upperUnixMS - before.lowerUnixMS)
        XCTAssertThrowsError(try a.at(.init(bootID: a.bootID, nanoseconds: a.receivedNS + 3_600_000_000_001), credentials: c))
        XCTAssertThrowsError(try a.at(.init(bootID: "boot:new", nanoseconds: a.receivedNS), credentials: c))
        XCTAssertThrowsError(try a.at(.init(bootID: a.bootID, nanoseconds: a.receivedNS - 1), credentials: c))
        let malformed = AnnotationClockAnchor(evidence: a.evidence, bootID: a.bootID, receivedNS: Int64.min)
        XCTAssertThrowsError(try malformed.at(.init(bootID: a.bootID, nanoseconds: 1), credentials: c))
        let tooSlow = AnnotationClockEvidence(request: a.evidence.request, observation: a.evidence.observation, rttNS: 2_000_000_001, elapsedNS: 0)
        XCTAssertThrowsError(try tooSlow.interval())
        let reading = try AnnotationMonotonicClock.read()
        XCTAssertGreaterThan(reading.nanoseconds, 0)
        XCTAssertTrue(reading.bootID.hasPrefix("boot:") || reading.bootID.hasPrefix("process:"))
    }

    func testSameBootRestoreAndRebootPreserveFrozenDraftButMakeNewTextUntimed() async throws {
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let clock = TestMonotonic(), c = try credentials(), context = try context()
        let first = try AnnotationOutbox(directory: dir, credentials: c, context: context, monotonic: clock.read)
        try await first.saveClockAnchor(anchor())
        let saved = try await first.saveDraft(text: "Offline bounded note", positionMM: [1, 2, 3])
        XCTAssertEqual(saved.state, .draft); XCTAssertNotNil(saved.draft.clock)
        clock.set(12_000_000_000)
        let sameBoot = try AnnotationOutbox(directory: dir, credentials: c, context: context, monotonic: clock.read)
        _ = try await sameBoot.clockEvidence()
        let restored = await sameBoot.entries()
        XCTAssertEqual(restored, [saved])
        clock.set(1, boot: "boot:restarted")
        let rebooted = try AnnotationOutbox(directory: dir, credentials: c, context: context, monotonic: clock.read)
        do { _ = try await rebooted.clockEvidence(); XCTFail("restored old boot") } catch { }
        let untimed = try await rebooted.saveDraft(text: "After reboot", positionMM: [0, 0, 0])
        XCTAssertEqual(untimed.state, .untimed); XCTAssertEqual(untimed.draft.authoredAtUnixMS, 0)
        XCTAssertNil(untimed.draft.clock); XCTAssertNil(untimed.signedRecord)
        let afterReboot = await rebooted.entries()
        XCTAssertEqual(afterReboot.first, saved)
    }

    func testRollbackAboveReceiptLatchesAndSurvivesAnOutboxWrite() async throws {
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let clock = TestMonotonic(), c = try credentials()
        let box = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        try await box.saveClockAnchor(anchor())
        clock.set(20_000_000_000)
        let timed = try await box.saveDraft(text: "Before rollback", positionMM: [0, 0, 0])
        clock.set(15_000_000_000)
        let unknown = try await box.saveDraft(text: "After rollback", positionMM: [0, 0, 0])
        XCTAssertEqual(timed.state, .draft); XCTAssertEqual(unknown.state, .untimed)
        clock.set(25_000_000_000)
        let restored = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        do { _ = try await restored.clockEvidence(); XCTFail("fault cleared after restart") } catch { }
        let saved = await restored.entries()
        XCTAssertEqual(saved, [timed, unknown])
    }

    func testNewOutboxFormatRefusesOldReadersAndV1PendingBodyMigratesUnchanged() async throws {
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let clock = TestMonotonic(), c = try credentials()
        let box = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        try await box.saveClockAnchor(anchor())
        _ = try await box.saveDraft(text: "v2 unsigned", positionMM: [0, 0, 0])
        let file = AnnotationFile(directory: dir, scopeID: box.scopeID)
        let stored = try JSONValue.decode(XCTUnwrap(file.load()))
        XCTAssertEqual(stored["version"]?.int, 2)
        // The shipped v1 reader requires saved.version == 1 before decoding entries.
        XCTAssertNotEqual(stored["version"]?.int, 1)

        let server = try LoopbackServer { request in
            if request.path == "/v1/annotations/epoch" { return .init(body: Data("{\"epoch\":7}".utf8)) }
            return .init(status: 503)
        }
        defer { server.stop() }
        let oldFixture = try AnnotationTests().credentials(url: server.url)
        let oldContext = try AnnotationTests().context()
        let oldBox = try AnnotationOutbox(directory: dir, credentials: oldFixture, context: oldContext)
        _ = try await oldBox.saveLegacyDraft(text: "Existing v1", positionMM: [0, 0, 0])
        let client = WorldClient(credentials: oldFixture, requestTime: { Int64(Date().timeIntervalSince1970 * 1000) })
        try? await oldBox.sync(using: client, now: Date())
        let previous = await oldBox.entries()
        XCTAssertNotNil(previous.first?.signedRecord)
        let oldFile = AnnotationFile(directory: dir, scopeID: oldBox.scopeID)
        let current = try XCTUnwrap(oldFile.load())
        var oldPayload = try JSONValue.decode(current).object!
        oldPayload["version"] = .integer(1)
        try oldFile.save(JSONValue.object(oldPayload).encoded(), replacing: WorldCredentials.digest(current))
        let reopened = try AnnotationOutbox(directory: dir, credentials: oldFixture, context: oldContext)
        try? await reopened.sync(using: client, now: Date())
        let migrated = await reopened.entries()
        XCTAssertEqual(migrated.first?.signedRecord, previous.first?.signedRecord)
        XCTAssertEqual(migrated.first?.draft, previous.first?.draft)
        XCTAssertEqual(try JSONValue.decode(XCTUnwrap(oldFile.load()))["version"]?.int, 2)
    }

    func testUntimedTextRemainsUntimedAfterAnchorAndClockUnknownDoesNotExpire() async throws {
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let clock = TestMonotonic(), c = try credentials()
        let outbox = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        let unknown = try await outbox.saveDraft(text: "Keep this untimed", positionMM: [0, 0, 0])
        try await outbox.saveClockAnchor(anchor())
        let timed = try await outbox.saveDraft(text: "Separate timed assertion", positionMM: [0, 0, 0])
        let entries = await outbox.entries()
        XCTAssertEqual(entries.first, unknown); XCTAssertEqual(entries.last, timed)
        XCTAssertThrowsError(try c.signAnnotation(unknown.draft, epoch: 1, sequence: 1))
        clock.set(4_000_000_000_000)
        let reopened = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        let reopenedEntries = await reopened.entries()
        XCTAssertEqual(reopenedEntries, entries)
    }
}

private struct TestClockGateway: Sendable {
    let template: JSONValue
    let root = Curve25519.Signing.PrivateKey()
    let gateway = Curve25519.Signing.PrivateKey()
    let sample: Int64
    init(template: JSONValue) { self.template = template; sample = template["observation"]!["observation"]!["sample_unix_ms"]!.int! }
    func grant(_ original: JSONValue) throws -> JSONValue {
        let placeholder = JSONValue.object(["grant": original, "signature": .string(Data(repeating: 0, count: 64).base64EncodedString())])
        let canonical = try GoAuthorityJSON.signedGrant(placeholder)
        let marker = canonical.range(of: Data(",\"signature\":".utf8), options: .backwards)!
        var bytes = Data("wendy.world.authority.v1/grant\0".utf8)
        bytes.append(canonical[9..<marker.lowerBound])
        return .object(["grant": original, "signature": .string(try root.signature(for: bytes).base64EncodedString())])
    }
    func credentials(url: URL) throws -> WorldCredentials {
        var config = template["credentials"]!.object!, pins = config["clock_trust"]!.object!
        config["grant"] = try grant(config["grant"]!["grant"]!)
        config["url"] = .string(url.absoluteString)
        pins["root_public_key"] = .string(root.publicKey.rawRepresentation.base64EncodedString())
        pins["gateway_key_sha256"] = .string(WorldCredentials.digest(gateway.publicKey.rawRepresentation))
        config["clock_trust"] = .object(pins)
        return try WorldCredentials(data: JSONValue.object(config).encoded())
    }
    func respond(_ request: LoopbackServer.Request, wrongNonce: Bool = false) throws -> Data {
        let proof = try JSONValue.decode(request.body)
        var observation = template["observation"]!["observation"]!.object!
        observation["request_sha256"] = .string(WorldCredentials.digest(try ClockWire.signed(proof, message: "request", keys: ClockWire.requestKeys)))
        observation["nonce"] = wrongNonce ? .string(String(repeating: "0", count: 64)) : proof["request"]!["nonce"]!
        var gatewayGrant = template["observation"]!["grant"]!["grant"]!.object!
        gatewayGrant["public_key"] = .string(gateway.publicKey.rawRepresentation.base64EncodedString())
        let signedGrant = try grant(.object(gatewayGrant))
        let bytes = try ClockWire.signingBytes(grant: signedGrant, message: ClockWire.message(.object(observation), keys: ClockWire.observationKeys), name: "observation")
        let signed = JSONValue.object(["version": .integer(1), "grant": signedGrant, "observation": .object(observation), "signature": .string(try gateway.signature(for: bytes).base64EncodedString())])
        return try ClockWire.signed(signed, message: "observation", keys: ClockWire.observationKeys)
    }
}

extension AnnotationClockTests {
    func testContributorBootstrapsBeforeReadUsingGatewayTimeInsteadOfHostWall() async throws {
        let gateway = TestClockGateway(template: try vector())
        let server = try LoopbackServer { request in
            if request.path == "/v1/annotations/clock" { return .init(body: (try? gateway.respond(request)) ?? Data()) }
            return .init(status: 503)
        }
        defer { server.stop() }
        let client = WorldClient(credentials: try gateway.credentials(url: server.url))
        do { _ = try await client.query(); XCTFail("expected read failure") } catch { }
        let requests = server.requests
        XCTAssertEqual(requests.map(\.path), ["/v1/annotations/clock", "/v1/components"])
        XCTAssertNil(requests[0].headers["x-world-authorization"])
        let signedRead = try JSONValue.decode(Data(base64Encoded: requests[1].headers["x-world-authorization"]!)!)
        XCTAssertLessThan(abs(signedRead["issued_at_unix_ms"]!.int! - gateway.sample), 2000)
    }

    func testSignedButWrongNonceBootstrapDoesNotSendNormalReads() async throws {
        let gateway = TestClockGateway(template: try vector())
        let server = try LoopbackServer { request in .init(body: (try? gateway.respond(request, wrongNonce: true)) ?? Data()) }
        defer { server.stop() }
        let client = WorldClient(credentials: try gateway.credentials(url: server.url))
        do { _ = try await client.query(); XCTFail("accepted wrong challenge") } catch { }
        XCTAssertEqual(server.requests.map(\.path), ["/v1/annotations/clock"])
        do { _ = try await client.currentClock(); XCTFail("retained bad time") } catch { }
    }

    func testV2PendingBytesAndClockEvidenceSurviveRetryWithLaterAnchor() async throws {
        let behavior = Responses()
        let server = try LoopbackServer { behavior.respond($0) }
        defer { server.stop() }
        let dir = directory(); defer { try? FileManager.default.removeItem(at: dir) }
        let clock = TestMonotonic(), c = try credentials(server.url), a = try anchor()
        let time = try a.evidence.interval().lowerUnixMS
        let client = WorldClient(credentials: c, requestTime: { time })
        let outbox = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        try await outbox.saveClockAnchor(a)
        let draft = try await outbox.saveDraft(text: "Frozen v2", positionMM: [3, 0, 2])
        try? await outbox.sync(using: client, now: Date(timeIntervalSince1970: Double(time)/1000))
        let pending = await outbox.entries()
        XCTAssertEqual(pending.first?.state, .pending)
        let body = try XCTUnwrap(pending.first?.signedRecord)
        clock.set(a.receivedNS + 15_000_000_000)
        let reopened = try AnnotationOutbox(directory: dir, credentials: c, context: context(), monotonic: clock.read)
        behavior.set(status: 200)
        try await reopened.sync(using: client, now: Date(timeIntervalSince1970: Double(time + 15_000)/1000))
        let published = await reopened.entries()
        XCTAssertEqual(published.first?.state, .published)
        XCTAssertEqual(published.first?.draft, draft.draft)
        XCTAssertEqual(published.first?.signedRecord, body)
        let sends = server.requests.filter { $0.path == "/v1/annotations" }
        XCTAssertEqual(sends.count, 2); XCTAssertEqual(sends.first?.body, sends.last?.body)
    }
}
