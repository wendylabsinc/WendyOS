import CryptoKit
import Foundation
import XCTest
@testable import WorldClient

@MainActor
final class TransportTests: XCTestCase {
    private func credentials(url: URL) throws -> WorldCredentials {
        var config = try ProtocolTests.vectors()[0]["credentials"]!.object!
        var signed = config["grant"]!.object!
        var grant = signed["grant"]!.object!
        // These unsigned local test grants are never accepted by a real node.
        grant["not_before_unix_ms"] = .integer(Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000))
        grant["not_after_unix_ms"] = .integer(Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000))
        signed["grant"] = .object(grant); config["grant"] = .object(signed)
        config["url"] = .string(url.absoluteString)
        return try WorldCredentials(data: JSONValue.object(config).encoded())
    }

    func testFixedReadEndpointsUseRealHTTPAndSignedHeaders() async throws {
        let server = try LoopbackServer { request in
            let value: String
            switch request.path {
            case "/v1/components": value = #"{"components":[],"cursor":"1"}"#
            case "/v1/sites": value = #"{"publications":[]}"#
            case "/v1/task-relay": value = #"{"entries":[],"last_known":true}"#
            default: return .init(status: 404)
            }
            return .init(body: Data(value.utf8))
        }
        defer { server.stop() }
        let client = WorldClient(credentials: try credentials(url: server.url))
        let snapshot = try await client.query()
        XCTAssertEqual(snapshot.cursor, "1")
        _ = try await client.sites(); _ = try await client.taskRelay()
        XCTAssertEqual(server.requests.map(\.path), ["/v1/components", "/v1/sites", "/v1/task-relay"])
        var nonces = Set<String>()
        for request in server.requests {
            let header = try XCTUnwrap(request.headers["x-world-authorization"])
            let value = try JSONValue.decode(XCTUnwrap(Data(base64Encoded: header)))
            XCTAssertEqual(value["operation"]?.string, "read")
            XCTAssertEqual(value["audience"]?.string, "node-3")
            XCTAssertEqual(value["body_sha256"]?.string, WorldCredentials.digest(Data()))
            nonces.insert(value["nonce"]!.string!)
        }
        XCTAssertEqual(nonces.count, 3)
    }

    func testRedirectDoesNotForwardCredentials() async throws {
        let destination = try LoopbackServer { _ in .init(body: Data("{}".utf8)) }
        defer { destination.stop() }
        let server = try LoopbackServer { _ in .init(status: 302, headers: ["Location": destination.url.absoluteString + "/stolen"]) }
        defer { server.stop() }
        var request = URLRequest(url: server.url)
        request.setValue("synthetic-test-authorization", forHTTPHeaderField: "X-World-Authorization")
        do { _ = try await BoundedRequest(limit: 128).run(request); XCTFail("redirect accepted") }
        catch { XCTAssertEqual(error as? WorldClientError, .redirectRefused) }
        XCTAssertEqual(server.requests.count, 1)
        XCTAssertTrue(destination.requests.isEmpty)
    }

    func testResponseLimitsApplyWithAndWithoutDeclaredLength() async throws {
        for declared in [true, false] {
            let server = try LoopbackServer { _ in .init(body: Data(repeating: 0x61, count: 256), includeLength: declared) }
            defer { server.stop() }
            do { _ = try await BoundedRequest(limit: 128).run(URLRequest(url: server.url)); XCTFail("oversized response accepted") }
            catch { XCTAssertEqual(error as? WorldClientError, .responseTooLarge) }
        }
    }

    func testHTTPErrorDoesNotReturnServerBody() async throws {
        let server = try LoopbackServer { _ in .init(status: 401, body: Data("sensitive server debug content".utf8)) }
        defer { server.stop() }
        do { _ = try await BoundedRequest(limit: 128).run(URLRequest(url: server.url)); XCTFail("HTTP error accepted") }
        catch { XCTAssertEqual(error as? WorldClientError, .httpStatus(401)); XCTAssertFalse(error.localizedDescription.contains("sensitive")) }
    }

    func testEvidenceChunksVerifyDigestAndStableMetadata() async throws {
        let bytes = Data((0..<(60 << 10)).map { UInt8($0 % 251) })
        let digest = WorldCredentials.digest(bytes)
        let until = Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000)
        for tamper in [false, true] {
            let server = try LoopbackServer { request in
                guard let value = try? JSONValue.decode(request.body), let offset = value["offset"]?.int,
                      offset >= 0, offset < bytes.count else { return .init(status: 400) }
                let end = min(Int(offset) + (48 << 10), bytes.count)
                var chunk = Data(bytes[Int(offset)..<end])
                if tamper && offset > 0 { chunk[0] ^= 0xff }
                let response: JSONValue = .object([
                    "sha256": .string(digest), "size": .integer(Int64(bytes.count)), "offset": .integer(offset),
                    "data": .string(chunk.base64EncodedString()), "content_type": .string("application/octet-stream"),
                    "retain_until_unix_ms": .integer(until)
                ])
                guard let encoded = try? response.encoded() else { return .init(status: 500) }
                return .init(body: encoded)
            }
            defer { server.stop() }
            let client = WorldClient(credentials: try credentials(url: server.url))
            if tamper {
                do { _ = try await client.downloadEvidence(digest: digest); XCTFail("corrupt evidence accepted") }
                catch { XCTAssertEqual(error as? WorldClientError, .invalidEvidence) }
            } else {
                let result = try await client.downloadEvidence(digest: digest)
                XCTAssertEqual(result.data, bytes); XCTAssertEqual(result.sha256, digest)
                XCTAssertEqual(result.contentType, "application/octet-stream")
            }
            XCTAssertEqual(server.requests.count, 2)
            for request in server.requests {
                XCTAssertEqual(request.path, "/v1/evidence/get")
                let auth = try JSONValue.decode(XCTUnwrap(Data(base64Encoded: request.headers["x-world-authorization"]!)))
                XCTAssertEqual(auth["operation"]?.string, "evidence-read")
                XCTAssertEqual(auth["body_sha256"]?.string, WorldCredentials.digest(request.body))
            }
        }
    }

    func testCrossSiteComponentsAndMalformedPartialResponseAreRejected() async throws {
        let server = try LoopbackServer { _ in .init(body: Data(#"{"components":[{"key":{"tenant":"other","site":"synthetic-office"}}],"cursor":"1"}"#.utf8)) }
        defer { server.stop() }
        let client = WorldClient(credentials: try credentials(url: server.url))
        do { _ = try await client.query(); XCTFail("cross-tenant response accepted") }
        catch { XCTAssertEqual(error as? WorldClientError, .invalidResponse) }
        XCTAssertThrowsError(try WorldSnapshot(data: Data(#"{"components":[42],"cursor":"1"}"#.utf8)))
    }

    func testEvidenceRejectsChangedMetadataMissingChunksAndExpiredRetention() async throws {
        let bytes = Data(repeating: 0x61, count: (48 << 10) + 4)
        let digest = WorldCredentials.digest(bytes)
        let until = Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000)
        for changed in ["content_type", "retain_until_unix_ms", "size", "offset", "data", "expired"] {
            let server = try LoopbackServer { request in
                guard let value = try? JSONValue.decode(request.body), let offset = value["offset"]?.int,
                      offset >= 0, offset < bytes.count else { return .init(status: 400) }
                let end = min(Int(offset) + (48 << 10), bytes.count)
                var response: [String: JSONValue] = [
                    "sha256": .string(digest), "size": .integer(Int64(bytes.count)), "offset": .integer(offset),
                    "data": .string(Data(bytes[Int(offset)..<end]).base64EncodedString()),
                    "content_type": .string("application/octet-stream"), "retain_until_unix_ms": .integer(until)
                ]
                if changed == "expired" { response["retain_until_unix_ms"] = .integer(1) }
                if offset > 0 {
                    switch changed {
                    case "content_type": response[changed] = .string("application/json")
                    case "retain_until_unix_ms": response[changed] = .integer(until - 1000)
                    case "size": response[changed] = .integer(Int64(bytes.count + 1))
                    case "offset": response[changed] = .integer(0)
                    case "data": response[changed] = .string("")
                    default: break
                    }
                }
                guard let encoded = try? JSONValue.object(response).encoded() else { return .init(status: 500) }
                return .init(body: encoded)
            }
            defer { server.stop() }
            let client = WorldClient(credentials: try credentials(url: server.url))
            do { _ = try await client.downloadEvidence(digest: digest); XCTFail("accepted evidence mutation: \(changed)") }
            catch { XCTAssertEqual(error as? WorldClientError, .invalidEvidence, changed) }
        }
    }

    func testSitePublicationMustMatchCredentialTenantAndSite() async throws {
        for tenant in ["lab", "other-organization"] {
            let body = try JSONValue.object(["publications": .array([
                .object(["component": .object([
                    "key": .object(["tenant": .string(tenant), "site": .string("synthetic-office")]),
                    "payload": .object(["site": .string("synthetic-office")])
                ])])
            ])]).encoded()
            let server = try LoopbackServer { _ in .init(body: body) }
            defer { server.stop() }
            let client = WorldClient(credentials: try credentials(url: server.url))
            if tenant == "lab" {
                let sites = try await client.sites()
                XCTAssertEqual(sites["publications"]?.array?.count, 1)
            } else {
                do { _ = try await client.sites(); XCTFail("Cross-tenant geographic publication accepted") }
                catch { XCTAssertEqual(error as? WorldClientError, .invalidResponse) }
            }
        }
    }
}
