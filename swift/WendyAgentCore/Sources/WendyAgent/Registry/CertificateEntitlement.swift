import Foundation
import SwiftASN1
import X509

/// A decoded rule from pki-core's entitlement certificate extension.
/// Category and permission remain open names from the PKI permission catalog.
struct CertificateEntitlement: Sendable {
    enum Effect: String, Sendable {
        case allow
        case deny
    }

    enum Target: Sendable, Equatable {
        case all
        case device(String)
        case site(String)
        case tag(String)

        func matchesDevice(_ device: String) -> Bool {
            switch self {
            case .all: true
            case .device(let value): value == device
            case .site, .tag: false
            }
        }

        // An offline certificate cannot establish site/tag membership.
        var requiresMembership: Bool {
            switch self {
            case .site, .tag: true
            case .all, .device: false
            }
        }

        fileprivate init(_ text: String) throws {
            if text == "all" || text == "*" {
                self = .all
                return
            }
            let parts = text.utf8.split(
                separator: UInt8(ascii: ":"),
                maxSplits: 1,
                omittingEmptySubsequences: false
            )
            guard parts.count == 2 else {
                throw BuildDeliveryContract.Failure("invalid entitlement target")
            }
            let value = String(decoding: parts[1], as: UTF8.self)
            guard !value.isEmpty, !value.utf8.contains(UInt8(ascii: "*")),
                value.unicodeScalars.allSatisfy({ scalar in
                    guard !scalar.properties.isWhitespace else { return false }
                    // Match pki-core's Go unicode.IsPrint/IsSpace target grammar.
                    switch scalar.properties.generalCategory {
                    case .control, .format, .unassigned, .surrogate, .privateUse: return false
                    default: return true
                    }
                })
            else { throw BuildDeliveryContract.Failure("invalid entitlement target") }
            switch String(decoding: parts[0], as: UTF8.self) {
            case "device": self = .device(value)
            case "site": self = .site(value)
            case "tag": self = .tag(value)
            default: throw BuildDeliveryContract.Failure("invalid entitlement target kind")
            }
        }
    }

    let category: String
    let permission: String
    let effect: Effect
    let target: Target

    // The effect is the final segment; targets such as tag:env:prod retain ':'.
    // Absent target, "all" and "*" all decode to .all.
    init(_ text: String) throws {
        // Delimiters are ASCII bytes in this UTF-8 protocol, even when followed
        // by combining marks that Swift groups with them into one Character.
        let fields = text.utf8.split(separator: UInt8(ascii: ":"), omittingEmptySubsequences: false)
            .map { String(decoding: $0, as: UTF8.self) }
        guard fields.count >= 4, fields[0] == "entitlement", !fields[1].isEmpty,
            !fields[2].isEmpty, let last = fields.last, let effect = Effect(rawValue: last)
        else { throw BuildDeliveryContract.Failure("invalid entitlement") }
        self.category = fields[1]
        self.permission = fields[2]
        self.effect = effect
        self.target =
            try fields.count == 4
            ? .all : Target(fields.dropFirst(3).dropLast().joined(separator: ":"))
    }

    static func fromCertificate(_ leaf: Certificate) throws -> [Self] {
        let extensions = leaf.extensions.filter { $0.oid == BuildDeliveryContract.entitlementOID }
        guard extensions.count == 1, let encoded = extensions.first?.value else {
            throw BuildDeliveryContract.Failure("operator requires one entitlement extension")
        }
        return try decode(encoded)
    }

    static func decode(_ encoded: ArraySlice<UInt8>) throws -> [Self] {
        guard !encoded.isEmpty, encoded.count <= 2048 else {
            throw BuildDeliveryContract.Failure("operator requires bounded deployment entitlements")
        }
        let node = try DER.parse(encoded)
        guard node.identifier == .sequence, case .constructed(let nodes) = node.content else {
            throw BuildDeliveryContract.Failure("malformed entitlements")
        }
        return try nodes.map { node in
            let bytes = try ASN1UTF8String(derEncoded: node).bytes
            guard let text = String(bytes: bytes, encoding: .utf8) else {
                throw BuildDeliveryContract.Failure("invalid entitlement UTF-8")
            }
            return try Self(text)
        }
    }
}
