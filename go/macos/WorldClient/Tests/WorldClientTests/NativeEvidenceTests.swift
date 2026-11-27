import Foundation
import XCTest
@testable import WorldClient

@MainActor
final class NativeEvidenceTests: XCTestCase {
    func reference(_ bytes: Data, device: String = "gateway-a", proof: String = String(repeating: "a", count: 64), mime: String = "image/png") throws -> NativeEvidenceReference {
        try NativeEvidenceReference(source: .object(["device": .string(device), "app": .string("capture-app"),
            "service": .string("camera-a"), "component_type": .string("wendy.sensor.capture.v1"), "stream": .string("camera-a")]),
            sha256: WorldCredentials.digest(bytes), size: bytes.count, contentType: mime, proofEnvelopeSHA256: proof)
    }
    func credentials(_ url: URL) throws -> WorldCredentials {
        var config = try ProtocolTests.vectors()[0]["credentials"]!.object!
        var signed = config["grant"]!.object!, grant = signed["grant"]!.object!
        grant["not_before_unix_ms"] = .integer(Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000))
        grant["not_after_unix_ms"] = .integer(Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000))
        signed["grant"] = .object(grant); config["grant"] = .object(signed); config["url"] = .string(url.absoluteString)
        return try WorldCredentials(data: JSONValue.object(config).encoded())
    }
    func directory() -> URL {
        let path = FileManager.default.temporaryDirectory.appendingPathComponent("native-media-" + UUID().uuidString)
        addTeardownBlock { try? FileManager.default.removeItem(at: path) }; return path
    }

    func testExactOwnerRangesThenOfflineReopenPreservesRetention() async throws {
        let bytes = Data((0..<(60 << 10)).map { UInt8($0 % 251) }), ref = try reference(bytes)
        let until = Int64(Date().addingTimeInterval(60).timeIntervalSince1970 * 1000)
        let server = try LoopbackServer { request in
            guard request.path == "/v1/native-evidence/get", let body = try? JSONValue.decode(request.body),
                  body["source"] == ref.source, body["sha256"]?.string == ref.sha256,
                  body["proof_envelope_sha256"] == nil, body["size"]?.int == Int64(bytes.count),
                  body["content_type"]?.string == ref.contentType, let offset = body["offset"]?.int,
                  let count = body["length"]?.int, count == min(48 << 10, bytes.count - Int(offset)) else { return .init(status: 400) }
            var fields = ref.raw.object!; fields["offset"] = .integer(offset)
            fields["data"] = .string(Data(bytes[Int(offset)..<(Int(offset)+Int(count))]).base64EncodedString())
            fields["retain_until_unix_ms"] = .integer(until)
            return .init(body: try! JSONValue.object(fields).encoded())
        }
        defer { server.stop() }
        let credentials = try credentials(server.url), client = WorldClient(credentials: credentials)
        let media = try await client.downloadNativeEvidence(ref)
        XCTAssertEqual(media.data, bytes); XCTAssertEqual(server.requests.count, 2)
        let root = directory(), cache = try InspectionMediaCache(directory: root, scopeID: credentials.scopeID)
        try await cache.store(media, reference: ref)
        server.stop()
        let reopened = try InspectionMediaCache(directory: root, scopeID: credentials.scopeID)
        let saved = try await reopened.load(reference: ref)
        XCTAssertEqual(saved?.data, bytes); XCTAssertEqual(saved?.retainUntil, media.retainUntil)
        XCTAssertEqual(server.requests.count, 2)
    }

    func testAnyRangeOwnerMetadataRetentionOrBytesMismatchReturnsNoMedia() async throws {
        let bytes = Data(repeating: 7, count: 50 << 10), ref = try reference(bytes)
        for change in ["owner", "size", "mime", "offset", "retention", "expired", "truncated", "digest"] {
            let until = Int64(Date().addingTimeInterval(60).timeIntervalSince1970 * 1000)
            let server = try LoopbackServer { request in
                let body = try! JSONValue.decode(request.body), offset = Int(body["offset"]!.int!)
                let end = min(offset + (48 << 10), bytes.count)
                var fields = ref.raw.object!
                fields["offset"] = .integer(Int64(offset)); fields["data"] = .string(Data(bytes[offset..<end]).base64EncodedString())
                fields["retain_until_unix_ms"] = .integer(until)
                switch change {
                case "owner": var owner=ref.source.object!; owner["device"] = .string("other"); fields["source"] = .object(owner)
                case "size": fields["size"] = .integer(Int64(bytes.count+1))
                case "mime": fields["content_type"] = .string("audio/wav")
                case "offset": fields["offset"] = .integer(Int64(offset+1))
                case "retention": if offset > 0 { fields["retain_until_unix_ms"] = .integer(until+1) }
                case "expired": fields["retain_until_unix_ms"] = .integer(1)
                case "truncated": fields["data"] = .string(Data(bytes[offset..<(end-1)]).base64EncodedString())
                default: fields["data"] = .string(Data(repeating: 8, count: end-offset).base64EncodedString())
                }
                return .init(body: try! JSONValue.object(fields).encoded())
            }
            do { _ = try await WorldClient(credentials: credentials(server.url)).downloadNativeEvidence(ref); XCTFail(change) }
            catch {
                if change == "expired" { XCTAssertEqual(error as? InspectionMediaCacheError, .expired) }
                else { XCTAssertEqual(error as? WorldClientError, .invalidEvidence, change) }
            }
            server.stop()
        }
    }

    func testOwnerAndCaptureProofSeparateCacheAndIdenticalRecapture() async throws {
        let bytes = Data(repeating: 1, count: 100), old = try reference(bytes)
        let fresh = try reference(bytes, proof: String(repeating: "b", count: 64)), other = try reference(bytes, device: "gateway-b")
        XCTAssertEqual(old.raw, fresh.raw); XCTAssertNotEqual(old.cacheKey, fresh.cacheKey)
        XCTAssertNotEqual(old.cacheKey, other.cacheKey)
        let now = Date(), until = now.addingTimeInterval(1), root = directory()
        let cache = try InspectionMediaCache(directory: root, scopeID: String(repeating: "c", count: 64))
        let original = VerifiedEvidence(data: bytes, sha256: old.sha256, contentType: old.contentType, retainUntil: until)
        try await cache.store(original, reference: old, now: now)
        let absent = try await cache.load(reference: other, now: now); XCTAssertNil(absent)
        let later = VerifiedEvidence(data: bytes, sha256: old.sha256, contentType: old.contentType, retainUntil: now.addingTimeInterval(60))
        try await cache.store(later, reference: fresh, now: until)
        let newCapture = try await cache.load(reference: fresh, now: until); XCTAssertEqual(newCapture?.retainUntil, later.retainUntil)
        do { _ = try await cache.load(reference: old, now: until); XCTFail("old capture renewed") }
        catch { XCTAssertEqual(error as? InspectionMediaCacheError, .expired) }
        do { try await cache.store(later, reference: old, now: until); XCTFail("old capture renewed by download") }
        catch { XCTAssertEqual(error as? InspectionMediaCacheError, .expired) }
        let different = try InspectionMediaCache(directory: root, scopeID: String(repeating: "d", count: 64))
        let unrelated = try await different.load(reference: fresh, now: until); XCTAssertNil(unrelated)
    }

    func testFiveActualFixtureOwnersAreBoundToExactProofs() throws {
        let components = try NativeInferenceTests.fixture()["snapshot"]!["components"]!.array!
        let views = try components.map { try NativeInference(component: WorldComponent(raw: $0)) }
        var streams = Set<String>()
        for view in views {
            for media in view.evidence {
                streams.insert(media.reference.source["stream"]!.string!)
                XCTAssertEqual(media.reference.proofEnvelopeSHA256, media.proof.envelopeSHA256)
                XCTAssertEqual(media.reference.source["device"], media.proof.source["device"])
                XCTAssertEqual(media.reference.source["component_type"]?.string, "wendy.sensor.capture.v1")
                XCTAssertEqual(try JSONDecoder().decode(NativeEvidenceReference.self, from: JSONEncoder().encode(media.reference)), media.reference)
            }
        }
        XCTAssertEqual(streams, ["camera-a", "camera-b", "people-front", "people-oblique", "audio-1"])
    }

    func testJPEGAndOriginalPNGRemainSeparateAndUnboundHazardEvidenceIsRefused() throws {
        let base = NativeInferenceTests(), components = try base.components()
        let track = try base.changedBody(components[1]) { body in
            var current = body["current"]!.array!, member = current[0].object!, image = member["image"]!.object!
            member["original_image"] = .object(image); image["content_type"] = .string("image/jpeg")
            image["sha256"] = .string(String(repeating: "f", count: 64)); member["image"] = .object(image)
            current[0] = .object(member); body["current"] = .array(current)
        }
        let view = try NativeInference(component: track)
        XCTAssertEqual(view.evidence[0].contentType, "image/jpeg"); XCTAssertFalse(view.evidence[0].isOriginalImage)
        XCTAssertEqual(view.evidence[1].contentType, "image/png"); XCTAssertTrue(view.evidence[1].isOriginalImage)
        XCTAssertEqual(view.evidence[0].proof, view.evidence[1].proof)
        let bad = try base.changedBody(components[2]) { body in
            var evidence = body["evidence"]!.array!, ref=evidence[0].object!
            ref["sha256"] = .string(String(repeating: "0", count: 64)); evidence[0] = .object(ref); body["evidence"] = .array(evidence)
        }
        XCTAssertThrowsError(try NativeInference(component: bad))
    }
}
