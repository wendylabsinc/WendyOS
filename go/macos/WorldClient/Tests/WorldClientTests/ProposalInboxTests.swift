import CryptoKit
import Foundation
import Darwin
import XCTest
@testable import WorldClient

private struct InboxFixture: Sendable {
    let root = Curve25519.Signing.PrivateKey()
    let key = Curve25519.Signing.PrivateKey()
    let now = Int64(Date().timeIntervalSince1970 * 1000)
    func origin(_ name: String = "origin-a") throws -> ProposalInboxOrigin {
        try ProposalInboxOrigin(json: .object(["origin": .string(name), "principal": .string("origin-device"), "assignee": .string("robot"), "run_id": .string("run"), "root_public_key": .string(root.publicKey.rawRepresentation.base64EncodedString())]))
    }
    func grant(operation: String, expired: Bool = false) throws -> JSONValue {
        let g: JSONValue = .object(["version": .integer(1), "id": .string("grant-" + operation), "tenant": .string("tenant"), "site": .string("site"), "domain": .string("simulation"), "principal": .string(operation == "replicate" ? "origin-device" : "author"), "kind": .string(operation == "replicate" ? "device" : "operator"), "source": .object(["device": .string("device"), "app": .string("app"), "service": .string("service"), "stream": .string("stream")]), "public_key": .string(key.publicKey.rawRepresentation.base64EncodedString()), "scopes": .array([.string(operation)]), "not_before_unix_ms": .integer(now - 100000), "not_after_unix_ms": .integer(expired ? now - 1000 : now + 600000)])
        let placeholder: JSONValue = .object(["grant": g, "signature": .string("")])
        let canonical = try GoAuthorityJSON.signedGrant(placeholder)
        let marker = canonical.range(of: Data(",\"signature\":".utf8), options: .backwards)!
        let signature = try root.signature(for: Data("wendy.world.authority.v1/grant\0".utf8) + canonical[canonical.index(canonical.startIndex, offsetBy: 9)..<marker.lowerBound])
        return .object(["grant": g, "signature": .string(signature.base64EncodedString())])
    }
    func request(body: Data, operation: String, origin: String, issued: Int64, expired: Bool = false) throws -> JSONValue {
        let g = try grant(operation: operation, expired: expired), nonce = "bounded-test-nonce"
        let fields: [(String, Data)] = [("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(try GoAuthorityJSON.signedGrant(g)))), ("audience", GoAuthorityJSON.string(origin)), ("operation", GoAuthorityJSON.string(operation)), ("body_digest", GoAuthorityJSON.string(WorldCredentials.digest(body))), ("nonce", GoAuthorityJSON.string(nonce)), ("issued", Data(String(issued).utf8)), ("expires", Data(String(issued + 1000).utf8))]
        let signature = try key.signature(for: Data("wendy.world.authority.v1/request\0".utf8) + GoAuthorityJSON.object(fields))
        return .object(["version": .integer(1), "grant": g, "audience": .string(origin), "operation": .string(operation), "body_sha256": .string(WorldCredentials.digest(body)), "nonce": .string(nonce), "issued_at_unix_ms": .integer(issued), "expires_at_unix_ms": .integer(issued + 1000), "signature": .string(signature.base64EncodedString())])
    }
    func row(id: String = "proposal-a", origin: String = "origin-a", sequence: UInt64 = 1, expired: Bool = false, expiryOverride: Int64? = nil, acceptedOverride: Int64? = nil, exportedOverride: Int64? = nil, purpose: String = "wendy.world.proposal-history.v1") throws -> Data {
        let proposed = now - 10000, accepted = acceptedOverride ?? (proposed + 100)
        let p: JSONValue = .object(["version": .integer(1), "id": .string(id), "tenant": .string("tenant"), "site": .string("site"), "run_id": .string("run"), "assignee": .string("robot"), "type": .string("wendy.nav.inspect.v1"), "parameters": .object(["target": .array([.integer(1), .number(0.2), .integer(0)])]), "summary": .string("Inspect the reported pump"), "not_after_unix_ms": .integer(expiryOverride ?? (expired ? now - 1500 : now + 60000))])
        let encoder = JSONEncoder(); encoder.outputFormatting = [.sortedKeys]
        let body = try encoder.encode(p), proof = try request(body: body, operation: "propose", origin: origin, issued: proposed, expired: expired)
        let record: JSONValue = .object(["sequence": .unsigned(sequence), "proposal": p, "principal": .string("author"), "kind": .string("operator"), "source": proof["grant"]!["grant"]!["source"]!, "grant_id": .string("grant-propose"), "class": .string("MOVE"), "accepted_at_unix_ms": .integer(accepted), "body_sha256": .string(WorldCredentials.digest(body)), "body": .string(body.base64EncodedString()), "proof": proof])
        let rawRecord = try record.encoded()
        let attestation = GoAuthorityJSON.object([("purpose", GoAuthorityJSON.string(purpose)), ("origin", GoAuthorityJSON.string(origin)), ("record", rawRecord)])
        let receipt = try request(body: attestation, operation: "replicate", origin: origin, issued: exportedOverride ?? (proposed + 500), expired: expired)
        return GoAuthorityJSON.object([("origin", GoAuthorityJSON.string(origin)), ("record", rawRecord), ("origin_proof", try receipt.encoded()), ("expired", Data(expired ? "true".utf8 : "false".utf8)), ("historical_provenance_verified", Data("true".utf8)), ("current_verification", GoAuthorityJSON.string("not_currently_verified")), ("origin_verification", GoAuthorityJSON.string(expired ? "grant_expired" : "current_grant_verified")), ("proposer_verification", GoAuthorityJSON.string(expired ? "grant_expired" : "current_grant_verified")), ("executable", Data("false".utf8))])
    }
    func page(_ rows: [Data], next: String = "", through: UInt64 = 1, origin: String = "origin-a") throws -> Data {
        var array = Data("[".utf8); for (i, row) in rows.enumerated() { if i > 0 { array.append(44) }; array.append(row) }; array.append(93)
        return GoAuthorityJSON.object([("origin", GoAuthorityJSON.string(origin)), ("proposals", array), ("next", GoAuthorityJSON.string(next)), ("progress", try JSONValue.object(["through": .unsigned(through), "last_sync_unix_ms": .integer(now), "via": .string("relay"), "caught_up_with_peer": .bool(true)]).encoded())])
    }
    func credentials(url: URL = URL(string: "http://127.0.0.1:9999")!) throws -> WorldCredentials {
        try WorldCredentials(data: JSONValue.object(["url": .string(url.absoluteString), "audience": .string("reader"), "grant": grant(operation: "read"), "private_key": .string((key.rawRepresentation + key.publicKey.rawRepresentation).base64EncodedString()), "allow_insecure_simulation": .bool(true)]).encoded())
    }
}

final class ProposalInboxTests: XCTestCase {
    func testHistoricalSignaturesSurviveExpiryWithoutClaimingCurrentAuthority() throws {
        let f = InboxFixture(), origin = try f.origin()
        let row = try RemoteProposal(data: f.row(expired: true), origin: origin, tenant: "tenant", site: "site")
        XCTAssertEqual(row.gatewayOriginVerification, "grant_expired")
        XCTAssertLessThan(row.expiresAtUnixMS, f.now)
        XCTAssertEqual(row.id, "origin-a:proposal-a")
        let other = try RemoteProposal(data: f.row(origin: "origin-b"), origin: f.origin("origin-b"), tenant: "tenant", site: "site")
        XCTAssertNotEqual(row.id, other.id)
    }
    func testTamperedImmutableProofScopeAndPurposeRejected() throws {
        let f = InboxFixture(), original = try f.row(), origin = try f.origin()
        for (before, after) in [("Inspect the reported pump", "Inspect a different pump"), ("\"sequence\":1", "\"sequence\":2"), ("\"run_id\":\"run\"", "\"run_id\":\"other\""), ("\"executable\":false", "\"executable\":true"), ("\"operation\":\"replicate\"", "\"operation\":\"propose\"")] {
            let modified = Data(String(decoding: original, as: UTF8.self).replacingOccurrences(of: before, with: after).utf8)
            XCTAssertNotEqual(modified, original)
            XCTAssertThrowsError(try RemoteProposal(data: modified, origin: origin, tenant: "tenant", site: "site"), before)
        }
        XCTAssertThrowsError(try RemoteProposal(data: original, origin: f.origin("origin-b"), tenant: "tenant", site: "site"))
        XCTAssertThrowsError(try RemoteProposal(data: original, origin: origin, tenant: "tenant", site: "other"))
        XCTAssertThrowsError(try RemoteProposal(data: original, origin: InboxFixture().origin(), tenant: "tenant", site: "site"))
    }
    func testCorrectlySignedImpossibleChronologyRejected() throws {
        let f = InboxFixture(), origin = try f.origin()
        for data in [try f.row(purpose: "live-admission"), try f.row(expiryOverride: f.now + 700000), try f.row(acceptedOverride: f.now - 8000), try f.row(exportedOverride: f.now - 10001), try f.row(expired: true, expiryOverride: f.now - 500, acceptedOverride: f.now - 900)] {
            XCTAssertThrowsError(try RemoteProposal(data: data, origin: origin, tenant: "tenant", site: "site"))
        }
    }
    func testCompletePaginationAndJournalNeverReplacePriorHistoryOnFailure() async throws {
        let f = InboxFixture(), origin = try f.origin(), credentials = try f.credentials()
        let firstRow = try f.row()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let a = try ProposalInboxJournal(directory: directory, credentials: credentials, origins: [origin])
        let stale = try ProposalInboxJournal(directory: directory, credentials: credentials, origins: [origin])
        let first = ProposalInboxDocument(origin: origin, pages: [try f.page([firstRow])], fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        try await a.install(first)
        do { try await stale.install(first); XCTFail("stale writer") } catch {}
        let partial = ProposalInboxDocument(origin: origin, pages: [try f.page([firstRow], next: "proposal-a", through: 2)], fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        do { try await a.install(partial); XCTFail("partial committed") } catch {}
        let mixed = ProposalInboxDocument(origin: origin, pages: [try f.page([firstRow], next: "proposal-a", through: 2), try f.page([f.row(id: "proposal-b", sequence: 2)], through: 3)], fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        do { try await a.install(mixed); XCTFail("mixed generation") } catch {}
        let omitted = ProposalInboxDocument(origin: origin, pages: [try f.page([], through: 1)], fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        do { try await a.install(omitted); XCTFail("history disappeared") } catch {}
        let oversized = ProposalInboxDocument(origin: origin, pages: Array(repeating: first.pages[0], count: 17), fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        do { try await a.install(oversized); XCTFail("oversized history committed") } catch {}
        let reopened = try ProposalInboxJournal(directory: directory, credentials: credentials, origins: [origin])
        let documents = await reopened.documents()
        XCTAssertEqual(documents.first?.pages, first.pages)
        let complete = ProposalInboxDocument(origin: origin, pages: [try f.page([firstRow], next: "proposal-a", through: 2), try f.page([f.row(id: "proposal-b", sequence: 2)], through: 2)], fetchedUnixMS: f.now, scopeID: credentials.scopeID)
        try await reopened.install(complete)
        XCTAssertEqual(try complete.rows(tenant: "tenant", site: "site").count, 2)
    }
    func testRealHTTPPaginationPreservesExactBytesAndRefusesChangedProgress() async throws {
        let f = InboxFixture(), origin = try f.origin()
        let first = try f.page([f.row()], next: "proposal-a", through: 2), second = try f.page([f.row(id: "proposal-b", sequence: 2)], through: 2)
        let server = try LoopbackServer { request in .init(body: request.path.contains("after=proposal-a") ? second : first) }; defer { server.stop() }
        let client = ProposalInboxClient(credentials: try f.credentials(url: server.url))
        let doc = try await client.read(origin: origin, pageSize: 1)
        XCTAssertEqual(doc.pages, [first, second]); XCTAssertEqual(server.requests.count, 2)
        XCTAssertFalse(server.requests[0].path.contains("after=")); XCTAssertTrue(server.requests[1].path.contains("after=proposal-a"))
        XCTAssertTrue(server.requests.allSatisfy { $0.headers["x-world-authorization"] != nil && $0.body.isEmpty })
        let changed = try f.page([f.row(id: "proposal-b", sequence: 2)], through: 3)
        let broken = try LoopbackServer { request in .init(body: request.path.contains("after=proposal-a") ? changed : first) }; defer { broken.stop() }
        do { _ = try await ProposalInboxClient(credentials: f.credentials(url: broken.url)).read(origin: origin, pageSize: 1); XCTFail("mixed read accepted") } catch {}
    }
    func testRealHTTPDenialAndOversizedResponseFailClosed() async throws {
        let f = InboxFixture(), origin = try f.origin()
        for response in [LoopbackServer.Response(status: 403), LoopbackServer.Response(body: Data(repeating: 32, count: (1 << 20) + 1))] {
            let server = try LoopbackServer { _ in response }; defer { server.stop() }
            do { _ = try await ProposalInboxClient(credentials: f.credentials(url: server.url)).read(origin: origin); XCTFail("denied or oversized reply accepted") } catch {}
        }
    }
    func testProfileExplicitPinsAndDuplicateOrigins() throws {
        let f = InboxFixture(), raw = try JSONValue.decode(JSONEncoder().encode(f.origin()))
        XCTAssertEqual(try ProposalInboxOrigin.profile(nil), [])
        XCTAssertThrowsError(try ProposalInboxOrigin.profile(.object(["version": .integer(1), "origins": .array([raw, raw])])))
        XCTAssertThrowsError(try ProposalInboxOrigin.profile(.object(["version": .integer(2), "origins": .array([raw])])))
    }
    func testActualGoReceiptWhenSupplied() throws {
        guard let path = ProcessInfo.processInfo.environment["PROPOSAL_INBOX_FIXTURE"] else { throw XCTSkip("actual Go interop fixture supplied by acceptance") }
        let root = URL(fileURLWithPath: path)
        let config = try JSONValue.decode(Data(contentsOf: root.appendingPathComponent("sdk-scope.json")))
        let origin = try ProposalInboxOrigin(json: config["origin"]!)
        let data = try Data(contentsOf: root.appendingPathComponent("sdk-page.json"))
        let page = try ProposalInboxPage(data, origin: origin, tenant: config["tenant"]!.string!, site: config["site"]!.string!)
        XCTAssertFalse(page.rows.isEmpty)
        let malformedURL = root.appendingPathComponent("signed-invalid-chronology.json")
        if FileManager.default.fileExists(atPath: malformedURL.path) {
            let malformed = try Data(contentsOf: malformedURL), row = try JSONValue.decode(malformed)["proposals"]!.array![0], proof = row["origin_proof"]!
            let message = Data("wendy.world.authority.v1/request\0".utf8) + GoAuthorityJSON.object([
                ("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(try GoAuthorityJSON.signedGrant(proof["grant"]!)))),
                ("audience", GoAuthorityJSON.string(proof["audience"]!.string!)), ("operation", GoAuthorityJSON.string("replicate")),
                ("body_digest", GoAuthorityJSON.string(proof["body_sha256"]!.string!)), ("nonce", GoAuthorityJSON.string(proof["nonce"]!.string!)),
                ("issued", Data(String(proof["issued_at_unix_ms"]!.int!).utf8)), ("expires", Data(String(proof["expires_at_unix_ms"]!.int!).utf8))])
            let key = try Curve25519.Signing.PublicKey(rawRepresentation: Data(base64Encoded: proof["grant"]!["grant"]!["public_key"]!.string!)!)
            XCTAssertTrue(key.isValidSignature(Data(base64Encoded: proof["signature"]!.string!)!, for: message), "malformed chronology has a valid origin signature under the native canonicalizer")
            XCTAssertThrowsError(try ProposalInboxPage(malformed, origin: origin, tenant: config["tenant"]!.string!, site: config["site"]!.string!))
        }
    }
}

extension MissionTests {
    private func inboxPreparedFixture(now: Date = Date(), lifetime: TimeInterval = 120, sampleTime: Date? = nil) throws -> (PreparedProposalHandoff, MissionProposal, MissionContext, Data) {
        let context = try MissionContext(json: planarContextJSON(), credentials: credentials(), pins: pins().value, navigationProfile: .planar(floorY: 0.24))
        var component = try JSONValue.decode(nativeTrigger().canonical)["component"]!.object!
        let sampled = sampleTime ?? now, until = sampled.addingTimeInterval(lifetime), physicalUntil = now.addingTimeInterval(lifetime), formatter = ISO8601DateFormatter()
        var timing = component["timing"]!.object!; timing["sample_time"] = .string(formatter.string(from: sampled)); component["timing"] = .object(timing); component["ttl_ns"] = .integer(Int64(lifetime * 1_000_000_000))
        var payload = component["payload"]!.object!, result = try JSONValue.decode(Data(base64Encoded: payload["result_json"]!.string!)!).object!
        result["current"] = .array(result["current"]!.array!.map { member in
            var m = member.object!; m["physical_until"] = .string(formatter.string(from: physicalUntil)); return .object(m)
        })
        result["observed_at"] = timing["sample_time"]; result["valid_until"] = .string(formatter.string(from: until)); payload["result_json"] = .string(try JSONValue.object(result).encoded().base64EncodedString()); component["payload"] = .object(payload)
        let trigger = try nativeTrigger(.object(component), target: [1.25, 0.24, 0])
        let destination = try MissionInspectionDestination(floorY: 0.24, maxOffsetM: 0.1, planarConfigSHA256: String(repeating: "1", count: 64))
        let proposal = try MissionProposal(view: nativeProposalView(digest: trigger.sha256, target: [1.25, 0.24, 0.05], inspectionDestination: destination), context: context, credentials: credentials())
        let raw = try JSONValue.object(["version": .integer(1), "proposal_id": .string(proposal.id), "original_envelope_sha256": .string(trigger.originalEnvelopeSHA256),
            "navigation_target": .array(proposal.target.map(JSONValue.number)), "inspection_destination": JSONValue.decode(destination.canonical()),
            "mission_input": .object(["version": .integer(1), "trigger_json": .string(trigger.canonical.base64EncodedString()), "trigger_sha256": .string(trigger.sha256), "target": .array(trigger.fixedTarget.map(JSONValue.number)), "envelope_sha256": .string(trigger.originalEnvelopeSHA256), "result_schema": .string(trigger.resultSchema), "sample_valid_until": .string(formatter.string(from: until)), "physical_valid_until": .string(formatter.string(from: physicalUntil))])]).encoded()
        return (try PreparedProposalHandoff(data: raw, proposal: proposal, context: context, nowUnixMS: Int64(now.timeIntervalSince1970 * 1000)), proposal, context, raw)
    }
    func testInboxPreparedDifferentSimulationAnchorKeepsOriginalPhysicalDeadline() throws {
        let physical = Date(timeIntervalSince1970: 1_795_593_600), sample = Date(timeIntervalSince1970: 1_767_225_600)
        let (prepared, proposal, context, raw) = try inboxPreparedFixture(now: physical, lifetime: 2, sampleTime: sample)
        XCTAssertEqual(prepared.notAfterUnixMS, 1_795_593_602_000)
        let draft = try MissionDraft(target: proposal.target, durationMS: 30000, context: context, proposal: proposal,
                                     trigger: prepared.trigger, inspectionDestination: prepared.destination,
                                     preparedUntilUnixMS: prepared.notAfterUnixMS, preparedHandoff: raw)
        XCTAssertEqual(try JSONDecoder().decode(MissionDraft.self, from: JSONEncoder().encode(draft)), draft)
        XCTAssertThrowsError(try PreparedProposalHandoff(data: raw, proposal: proposal, context: context, nowUnixMS: prepared.notAfterUnixMS))
        var changed = try JSONValue.decode(raw).object!, input = changed["mission_input"]!.object!
        input["physical_valid_until"] = .string("2030-01-01T00:00:00Z"); changed["mission_input"] = .object(input)
        let capped = try PreparedProposalHandoff(data: JSONValue.object(changed).encoded(), proposal: proposal, context: context, nowUnixMS: Int64(physical.timeIntervalSince1970 * 1000))
        XCTAssertEqual(capped.notAfterUnixMS, prepared.notAfterUnixMS, "embedded original physical bounds cannot be extended by file metadata")
        input["sample_valid_until"] = .string("2030-01-01T00:00:00Z"); changed["mission_input"] = .object(input)
        XCTAssertThrowsError(try PreparedProposalHandoff(data: JSONValue.object(changed).encoded(), proposal: proposal, context: context, nowUnixMS: Int64(physical.timeIntervalSince1970 * 1000)))
    }
    func testInboxUnsignedDraftPreservesSupportedFullPlanarTarget() throws {
        let (prepared, proposal, context, _) = try inboxPreparedFixture()
        XCTAssertThrowsError(try proposal.unsignedDraft(durationMS: 30000, context: context))
        let draft = try proposal.unsignedDraft(durationMS: 30000, context: context, prepared: prepared)
        XCTAssertEqual(draft.target, [1.25, 0.24, 0.05]); XCTAssertEqual(draft.trigger?.canonical, prepared.trigger.canonical)
        XCTAssertEqual(draft.inspectionDestination, prepared.destination); XCTAssertEqual(draft.preparedUntilUnixMS, prepared.notAfterUnixMS)
        XCTAssertEqual(try JSONDecoder().decode(MissionDraft.self, from: JSONEncoder().encode(draft)), draft)
        XCTAssertEqual(try MissionNavigationProfile.imported(.object(["kind": .string("planar"), "floor_y": .number(0.24)])), .planar(floorY: 0.24))
        XCTAssertThrowsError(try MissionNavigationProfile.imported(.object(["kind": .string("planar"), "floor_y": .null])))
        let plain = try MissionProposal(view: nativeProposalView(digest: nil, target: [1.25, 0.24, 0.05]), context: context, credentials: credentials())
        XCTAssertFalse(plain.canDraftWithoutPreparedHandoff); XCTAssertThrowsError(try plain.unsignedDraft(durationMS: 30000, context: context))
    }
    func testInboxPreparedHandoffRejectsMismatchExpiryAndDeadlineExtension() throws {
        let (prepared, proposal, context, raw) = try inboxPreparedFixture()
        XCTAssertThrowsError(try PreparedProposalHandoff(data: raw, proposal: proposal, context: context, nowUnixMS: prepared.notAfterUnixMS))
        for mode in ["proposal", "target", "digest", "deadline", "envelope", "unknown"] {
            var value = try JSONValue.decode(raw).object!, input = value["mission_input"]!.object!
            switch mode {
            case "proposal": value["proposal_id"] = .string("other")
            case "target": value["navigation_target"] = .array([.integer(0), .number(0.24), .integer(0)])
            case "digest": input["trigger_sha256"] = .string(String(repeating: "0", count: 64))
            case "deadline": input["sample_valid_until"] = .string("2020-01-01T00:00:00Z")
            case "unknown": input["endpoint"] = .string("http://unconfigured.invalid")
            default: value["original_envelope_sha256"] = .string(String(repeating: "0", count: 64))
            }
            value["mission_input"] = .object(input)
            XCTAssertThrowsError(try PreparedProposalHandoff(data: JSONValue.object(value).encoded(), proposal: proposal, context: context), mode)
        }
        let draft = try proposal.unsignedDraft(durationMS: 30000, context: context, prepared: prepared)
        var changed = try JSONValue.decode(JSONEncoder().encode(draft)).object!
        changed["preparedUntilUnixMS"] = .integer(prepared.notAfterUnixMS + 100000)
        XCTAssertThrowsError(try JSONDecoder().decode(MissionDraft.self, from: JSONValue.object(changed).encoded()))
        changed.removeValue(forKey: "preparedUntilUnixMS"); changed.removeValue(forKey: "preparedHandoff")
        XCTAssertThrowsError(try JSONDecoder().decode(MissionDraft.self, from: JSONValue.object(changed).encoded()), "removing both fields must not downgrade the explicit prepared mode")
    }
    func testInboxPreparedJournalRequiresModeAndLegacyMigrationRemainsReadable() async throws {
        let (prepared, proposal, context, _) = try inboxPreparedFixture(), credentials = try credentials()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try MissionJournal(directory: directory, credentials: credentials, context: context)
        let draft = try proposal.unsignedDraft(durationMS: 30000, context: context, prepared: prepared)
        try await journal.saveDraft(draft)
        let file = MissionFile(directory: directory, scopeID: journal.scopeID)
        let saved = try XCTUnwrap(file.load()), digest = WorldCredentials.digest(saved)
        var root = try JSONValue.decode(saved).object!, entries = root["entries"]!.array!, entry = entries[0].object!, raw = entry["draft"]!.object!
        XCTAssertEqual(root["version"]?.int, 2)
        for key in ["preparationMode", "preparedHandoff", "preparedUntilUnixMS"] { raw.removeValue(forKey: key) }
        entry["draft"] = .object(raw); entries[0] = .object(entry); root["entries"] = .array(entries)
        try file.save(JSONValue.object(root).encoded(), replacing: digest)
        XCTAssertThrowsError(try MissionJournal(directory: directory, credentials: credentials, context: context), "v2 cannot silently infer legacy mode")
        // A real version-one legacy draft remains readable and gains explicit
        // mode the next time the normal journal writer persists it.
        var legacy = try JSONValue.decode(JSONEncoder().encode(MissionDraft(target: [1, 0.24, 0], durationMS: 30000, context: context))).object!
        legacy.removeValue(forKey: "preparationMode")
        XCTAssertEqual(try JSONDecoder().decode(MissionDraft.self, from: JSONValue.object(legacy).encoded()).preparationMode, .legacyTrusted)
        let legacyDirectory = directory.appendingPathComponent("legacy"), legacyFile = MissionFile(directory: legacyDirectory, scopeID: journal.scopeID)
        var legacyRoot = root; legacyRoot["version"] = .integer(1); legacyRoot["entries"] = .array([.object(["draft": .object(legacy), "state": .string("draft")])])
        try legacyFile.save(JSONValue.object(legacyRoot).encoded(), replacing: nil)
        let restored = try MissionJournal(directory: legacyDirectory, credentials: credentials, context: context)
        let rows = await restored.entries(); XCTAssertEqual(rows.count, 1); XCTAssertEqual(rows[0].draft.preparationMode, .legacyTrusted)
        try await restored.saveDraft(MissionDraft(target: [1.1, 0.24, 0], durationMS: 30000, context: context))
        let migrated = try JSONValue.decode(XCTUnwrap(legacyFile.load()))
        XCTAssertEqual(migrated["version"]?.int, 2)
        XCTAssertTrue(migrated["entries"]!.array!.allSatisfy { $0["draft"]?["preparationMode"]?.string == "legacyTrusted" })
    }
    func testInboxPreparedFractionalCarryKeepsExactSampleBoundAndPhysicalDeadline() throws {
        let physical = Date(timeIntervalSince1970: 1_795_593_600)
        let (base, _, context, raw) = try inboxPreparedFixture(now: physical, lifetime: 120)
        let cases: [(String, Int64, Int64, String)] = [
            // Exact original timestamps retained by the two real rising runs.
            ("2026-11-25T09:52:18.790705Z", 3_588_434_499, 0, "2026-11-25T09:52:22.379139499Z"),
            ("2026-11-25T10:54:59.404085+01:00", 3_005_986_000, 0, "2026-11-25T09:55:02.410071Z"),
            // A one-nanosecond conservative lifetime crosses a second boundary.
            ("2026-01-01T00:00:00.999999999Z", 2, 1, "2026-01-01T00:00:01Z")
        ]
        for (sample, ttl, error, until) in cases {
            var component = try JSONValue.decode(base.trigger.canonical)["component"]!.object!
            var timing = component["timing"]!.object!
            timing["sample_time"] = .string(sample); timing["error_bound_ns"] = .integer(error)
            component["timing"] = .object(timing); component["ttl_ns"] = .integer(ttl)
            var payload = component["payload"]!.object!
            var result = try JSONValue.decode(Data(base64Encoded: payload["result_json"]!.string!)!).object!
            result["observed_at"] = .string(sample); result["valid_until"] = .string(until)
            payload["result_json"] = .string(try JSONValue.object(result).encoded().base64EncodedString()); component["payload"] = .object(payload)
            let trigger = try nativeTrigger(.object(component), target: base.trigger.fixedTarget)
            let proposal = try MissionProposal(view: nativeProposalView(digest: trigger.sha256, target: [1.25, 0.24, 0.05], inspectionDestination: base.destination), context: context, credentials: credentials())
            var value = try JSONValue.decode(raw).object!, input = value["mission_input"]!.object!
            input["trigger_json"] = .string(trigger.canonical.base64EncodedString()); input["trigger_sha256"] = .string(trigger.sha256)
            input["sample_valid_until"] = .string(until); value["mission_input"] = .object(input)
            let updated = try JSONValue.object(value).encoded()
            let prepared = try PreparedProposalHandoff(data: updated, proposal: proposal, context: context, nowUnixMS: Int64(physical.timeIntervalSince1970 * 1000))
            XCTAssertEqual(prepared.notAfterUnixMS, base.notAfterUnixMS, "sample arithmetic must not change the original physical deadline")
            let draft = try MissionDraft(target: proposal.target, durationMS: 30000, context: context, proposal: proposal, trigger: trigger, inspectionDestination: base.destination, preparedUntilUnixMS: prepared.notAfterUnixMS, preparedHandoff: updated)
            XCTAssertEqual(try JSONDecoder().decode(MissionDraft.self, from: JSONEncoder().encode(draft)), draft)
            XCTAssertThrowsError(try PreparedProposalHandoff(data: updated, proposal: proposal, context: context, nowUnixMS: prepared.notAfterUnixMS))
            input["sample_valid_until"] = .string("2030-01-01T00:00:00Z"); value["mission_input"] = .object(input)
            XCTAssertThrowsError(try PreparedProposalHandoff(data: JSONValue.object(value).encoded(), proposal: proposal, context: context, nowUnixMS: Int64(physical.timeIntervalSince1970 * 1000)))
        }
    }
    func testInboxGoNanosecondDeadlinesFloorWithoutRenewal() throws {
        XCTAssertEqual(try PreparedProposalHandoff.unixMilliseconds("2026-11-25T07:00:00.999999999Z"), 1795590000999)
        XCTAssertEqual(try PreparedProposalHandoff.unixMilliseconds("2026-11-25T08:00:00.000999999+01:00"), 1795590000000)
        XCTAssertEqual(try PreparedProposalHandoff.unixMilliseconds("2026-11-25T07:00:00Z"), 1795590000000)
        XCTAssertThrowsError(try PreparedProposalHandoff.unixMilliseconds("2026-11-25T07:00:00.0000000001Z"))
    }
    func testInboxStoppedLoopbackRetainsDescriptorUntilHandlerExits() async throws {
        let entered = DispatchSemaphore(value: 0), release = DispatchSemaphore(value: 0)
        let old = try LoopbackServer { _ in
            entered.signal()
            _ = release.wait(timeout: .now() + 5)
            return .init(status: 503)
        }
        defer { release.signal(); old.stop() }
        let requestURL = old.url
        async let first = URLSession.shared.data(from: requestURL)
        XCTAssertEqual(entered.wait(timeout: .now() + 2), .success)
        old.stop()
        // An in-flight handler still owns the accept loop. Closing its listener
        // here lets a later fixture reuse the fd before that loop exits.
        XCTAssertNotEqual(fcntl(old.socketFD, F_GETFD), -1)
        let next = try LoopbackServer { _ in .init(body: Data("next fixture".utf8)) }
        defer { next.stop() }
        XCTAssertNotEqual(next.socketFD, old.socketFD)
        release.signal()
        let (_, response) = try await first
        XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 503)
        let (body, nextResponse) = try await URLSession.shared.data(from: next.url)
        XCTAssertEqual((nextResponse as? HTTPURLResponse)?.statusCode, 200)
        XCTAssertEqual(body, Data("next fixture".utf8))
        XCTAssertEqual(old.requests.count, 1)
        XCTAssertEqual(next.requests.count, 1)
    }
    func testInboxPreparedDeadlineRecheckedByAuthenticatedClockBeforeSigning() async throws {
        let v = try fixture(), epoch = v["now_unix_ms"]!.int!
        let (prepared, proposal, context, _) = try inboxPreparedFixture(now: Date(timeIntervalSince1970: Double(epoch - 5000) / 1000), lifetime: 2)
        let view = try nativeProposalView(digest: prepared.trigger.sha256, target: proposal.target, inspectionDestination: prepared.destination)
        let server = try LoopbackServer { request in
            do {
                if request.path == "/v1/tasks/clock" { return try inboxClockFixtureResponse(request, fixture: v) }
                if request.path == "/v1/tasks/catalog" { return .init(body: try JSONValue.object(["assignee": v["spec"]!["assignee"]!, "domain": .string("simulation"), "run_id": .string("run-native"), "types": .array([.object(["type": .string("wendy.nav.inspect.v1"), "class": .string("MOVE")])])]).encoded()) }
                if request.path == "/v1/proposals/native-trigger-proposal" { return .init(body: try view.encoded()) }
                return .init(status: 404)
            } catch { return .init(status: 500) }
        }
        defer { server.stop() }
        let credentials = try MissionCredentials(data: JSONValue.object(["url": .string(server.url.absoluteString), "audience": .string("node-1"), "grant": v["operator_grant"]!, "private_key": v["operator_private_key"]!, "allow_insecure_simulation": .bool(true)]).encoded())
        let client = try MissionClient(credentials: credentials, context: context.signed, pins: pins(), navigationProfile: .planar(floorY: 0.24))
        let draft = try MissionDraft(target: proposal.target, durationMS: 30000, context: context, proposal: proposal, trigger: prepared.trigger, inspectionDestination: prepared.destination, preparedUntilUnixMS: prepared.notAfterUnixMS, preparedHandoff: prepared.raw)
        let operation = try await client.beginMissionOperation()
        do { _ = try await client.prepareInstruction(draft: draft, operation: operation); XCTFail("expired prepared source signed") }
        catch { XCTAssertEqual(error as? MissionError, .expired, "Unexpected signing error: \(error)") }
        XCTAssertFalse(server.requests.contains { $0.path == "/v1/tasks" })
    }

}

private func inboxClockFixtureResponse(_ request: LoopbackServer.Request, fixture v: JSONValue) throws -> LoopbackServer.Response {
    let fields = try JSONValue.decode(request.body)["request"]!
    var observation = v["clock_observation"]!["observation"]!.object!
    observation["purpose"] = fields["purpose"]!; observation["nonce"] = fields["nonce"]!
    observation["request_sha256"] = .string(WorldCredentials.digest(request.body))
    let message = try ClockWire.message(.object(observation), keys: MissionClockWire.observationKeys)
    let grant = v["clock_server_grant"]!, key = try Curve25519.Signing.PrivateKey(rawRepresentation: Data(repeating: 43, count: 32))
    let signature = try key.signature(for: MissionClockWire.signing(grant: grant, message: message, name: "observation"))
    return .init(body: GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", try GoAuthorityJSON.signedGrant(grant)), ("observation", message), ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))]))
}
