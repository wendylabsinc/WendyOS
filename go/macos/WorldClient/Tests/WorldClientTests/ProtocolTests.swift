import CryptoKit
import Foundation
import XCTest
@testable import WorldClient

final class ProtocolTests: XCTestCase {
    static func vectors() throws -> [JSONValue] {
        let url = Bundle.module.url(forResource: "native-request", withExtension: "json", subdirectory: "Fixtures")!
        return try JSONValue.decode(Data(contentsOf: url))["vectors"]!.array!
    }

    func testGoGeneratedSigningVectors() throws {
        for vector in try Self.vectors() {
            let credentials = try WorldCredentials(data: vector["credentials"]!.encoded())
            XCTAssertEqual(String(data: credentials.canonicalGrant, encoding: .utf8), vector["expected_grant_json"]!.string)
            let result = try credentials.signedReadAuthorization(
                operation: vector["operation"]!.string!,
                body: Data(base64Encoded: vector["body_base64"]!.string!)!,
                nonce: vector["nonce"]!.string!, issuedAtUnixMS: vector["issued_at_unix_ms"]!.int!,
                lifetimeMS: vector["lifetime_ms"]!.int!)
            let signed = try JSONValue.decode(result)
            let signature = Data(base64Encoded: signed["signature"]!.string!)!
            let message = Data(base64Encoded: vector["expected_signing_bytes_base64"]!.string!)!
            XCTAssertTrue(credentials.key.publicKey.isValidSignature(signature, for: message), vector["name"]!.string!)
            // CryptoKit may randomize Ed25519 signing. Its signature must verify
            // against Go's exact signing bytes; all unsigned fields must match.
            var fields = signed.object!
            var expected = vector["expected_signed_request"]!.object!
            fields.removeValue(forKey: "signature"); expected.removeValue(forKey: "signature")
            XCTAssertEqual(fields, expected)
            let goSignature = Data(base64Encoded: vector["expected_signed_request"]!["signature"]!.string!)!
            XCTAssertTrue(credentials.key.publicKey.isValidSignature(goSignature, for: message))
        }
    }

    func testOnlyExplicitSimulationReadCredentialsAreImported() throws {
        let original = try Self.vectors()[0]["credentials"]!
        func changed(_ mutate: (inout [String: JSONValue]) -> Void) throws -> Data {
            var config = original.object!
            var signed = config["grant"]!.object!
            var grant = signed["grant"]!.object!
            mutate(&grant)
            signed["grant"] = .object(grant); config["grant"] = .object(signed)
            return try JSONValue.object(config).encoded()
        }
        XCTAssertThrowsError(try WorldCredentials(data: changed { $0["domain"] = .string("hardware") }))
        XCTAssertThrowsError(try WorldCredentials(data: changed { $0["scopes"] = .array([.string("read"), .string("mission")]) }))
        XCTAssertThrowsError(try WorldCredentials(data: changed { $0["scopes"] = .array([.string("read"), .string("read")]) }))
        XCTAssertThrowsError(try WorldCredentials(data: changed { $0["future_authority_field"] = .string("unknown") }))
        var config = original.object!
        config["allow_insecure_simulation"] = .bool(false)
        XCTAssertThrowsError(try WorldCredentials(data: JSONValue.object(config).encoded()))
        for endpoint in ["https://user:password@localhost", "https://localhost/path", "https://localhost?token=x", "https://localhost/#x"] {
            config = original.object!; config["url"] = .string(endpoint)
            XCTAssertThrowsError(try WorldCredentials(data: JSONValue.object(config).encoded()))
        }
        config = original.object!; config["private_key"] = .string(Data(repeating: 0, count: 64).base64EncodedString())
        XCTAssertThrowsError(try WorldCredentials(data: JSONValue.object(config).encoded()))
    }

    func testSigningDoesNotAuthorizeOtherOperationsOrExpiredReads() throws {
        let credentials = try WorldCredentials(data: Self.vectors()[0]["credentials"]!.encoded())
        for operation in ["execute", "publish", "stop", "mission", "task-status"] {
            XCTAssertThrowsError(try credentials.signedReadAuthorization(operation: operation, nonce: "test_nonce_0123456789", issuedAtUnixMS: 1794312000000))
        }
        XCTAssertThrowsError(try credentials.signedReadAuthorization(operation: "read", nonce: "test_nonce_0123456789", issuedAtUnixMS: 1794315600000))
        XCTAssertThrowsError(try credentials.signedReadAuthorization(operation: "read", nonce: "invalid nonce has spaces", issuedAtUnixMS: 1794312000000))
    }

    func testJSONPreservesWideIntegersAndUnknownFields() throws {
        let data = Data(#"{"sequence":18446744073709551615,"nanoseconds":1794312000123456789,"unknown":[null,true,"東京"]}"#.utf8)
        let value = try JSONValue.decode(data)
        XCTAssertEqual(value["sequence"], .unsigned(UInt64.max))
        XCTAssertEqual(value["nanoseconds"]?.int, 1794312000123456789)
        XCTAssertEqual(try JSONValue.decode(value.encoded()), value)
    }
}
