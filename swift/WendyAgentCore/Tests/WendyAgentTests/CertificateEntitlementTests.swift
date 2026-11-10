import Foundation
import SwiftASN1
import Testing

@testable import WendyAgentCore

@Suite("Certificate entitlement grammar and deployment policy")
struct CertificateEntitlementTests {
    private static func encoded(_ rules: [String]) throws -> ArraySlice<UInt8> {
        var serializer = DER.Serializer()
        try serializer.appendConstructedNode(identifier: .sequence) { coder in
            for rule in rules {
                try coder.serialize(ASN1UTF8String(contentBytes: ArraySlice(rule.utf8)))
            }
        }
        return ArraySlice(serializer.serializedBytes)
    }

    @Test("shared Go/Swift grammar and offline authorization vectors")
    func conformance() throws {
        struct Vectors: Decodable {
            struct Entry: Decodable {
                var name: String
                var entitlements: [String]
                var valid: Bool
                var allowed: Bool
            }
            var device: String
            var cases: [Entry]
        }
        let path = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .appendingPathComponent(
                "../../../../go/internal/shared/builddelivery/testdata/entitlements.json"
            )
            .standardizedFileURL
        let vectors = try JSONDecoder().decode(Vectors.self, from: Data(contentsOf: path))
        for entry in vectors.cases {
            let data = try Self.encoded(entry.entitlements)
            if entry.valid {
                let rules = try #require(try? CertificateEntitlement.decode(data), "\(entry.name)")
                let allowed: Bool
                do {
                    try BuildDeliveryContract.authorizeDeployment(rules, device: vectors.device)
                    allowed = true
                } catch {
                    allowed = false
                }
                #expect(allowed == entry.allowed, "\(entry.name)")
            } else {
                #expect(throws: (any Error).self, "\(entry.name)") {
                    try CertificateEntitlement.decode(data)
                }
            }
        }
    }

    @Test("a colon in a tag name survives parsing")
    func tagWithColons() throws {
        let rule = try CertificateEntitlement("entitlement:deployment:create:tag:env:prod:deny")
        #expect(rule.target == .tag("env:prod"))
        #expect(rule.effect == .deny)
    }

    @Test("malformed or oversized DER cannot supply entitlement rules")
    func malformedDER() throws {
        let valid = try Self.encoded(["entitlement:deployment:create:allow"])
        let invalid: [[UInt8]] = [
            [], [UInt8](repeating: 0, count: 2049), Array(valid) + [0],
            [0x0c, 0],  // Not a SEQUENCE.
            [0x30, 2, 0x04, 0],  // OCTET STRING instead of UTF8String.
            [0x30, 2, 0x2c, 0],  // Constructed UTF8String.
            [0x30, 3, 0x0c, 1, 0xff],  // Invalid UTF-8.
        ]
        for bytes in invalid {
            #expect(throws: (any Error).self) {
                try CertificateEntitlement.decode(ArraySlice(bytes))
            }
        }
    }
}
