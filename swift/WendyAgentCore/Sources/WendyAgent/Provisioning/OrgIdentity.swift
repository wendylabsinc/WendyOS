import Foundation
import X509

/// Extracts the Wendy identity carried by a certificate, mirroring the Go
/// agent's `certs.IdentityFromCert`.
///
/// Resolution order, SPIFFE first:
///  1. The tenant SPIFFE principal SAN
///     `spiffe://wendy.sh/tenant/<uuid>/<kind>/<name>` (authoritative; at most
///     one). This is what pki-core stamps on everything it issues and
///     re-stamps on everything it renews.
///  2. A SAN URI beginning with `urn:wendy:org:` (legacy old-chain reading; at
///     most one). On a transitional leaf carrying both, the principal decides
///     who the caller is and the URN contributes only its organization.
///  3. The CommonName `sh/wendy/<org>/<asset>` (legacy device-cert fallback).
///  4. Otherwise: no identity (a legacy cert that carries no claim at all —
///     e.g. the `wendy` CLI's `wendy/user/<uid>` user certificate).
///
/// The distinction between "no identity" (`nil`) and "malformed claim" (a thrown
/// error) matters: the mTLS enforcement gate allows no-identity certs under
/// grace mode but always rejects a cert whose claim is present-but-broken.
enum OrgIdentity {
    /// The tenant an identity belongs to.
    ///
    /// pki-core scopes everything it mints by tenant UUID; the `Int32` org
    /// survives only on old-chain certificates. Both are carried because a
    /// transitional leaf presents both SANs, and a comparison is only
    /// meaningful between two scopes expressed in the same terms.
    struct Scope: Equatable, Sendable {
        var tenantUUID: String?
        var orgID: Int32?

        /// Whether this scope names anything at all.
        var isKnown: Bool { tenantUUID != nil || orgID != nil }

        /// Whether two scopes are expressed in a shared vocabulary, so that a
        /// `matches` of `false` means "a different tenant" rather than "no way
        /// to tell".
        func comparable(with other: Scope) -> Bool {
            (tenantUUID != nil && other.tenantUUID != nil) || (orgID != nil && other.orgID != nil)
        }

        /// Whether two scopes provably name the same tenant. Tenant UUIDs win
        /// when both sides have one; otherwise the legacy org is compared when
        /// both sides have one. A pair with no shared vocabulary is not a
        /// match: no mapping exists between a tenant UUID and an `Int32` org.
        func matches(_ other: Scope) -> Bool {
            if let mine = tenantUUID, let theirs = other.tenantUUID { return mine == theirs }
            if let mine = orgID, let theirs = other.orgID { return mine == theirs }
            return false
        }

        var description: String {
            switch (tenantUUID, orgID) {
            case (.some(let tenant), .some(let org)): return "tenant \(tenant) (org \(org))"
            case (.some(let tenant), .none): return "tenant \(tenant)"
            case (.none, .some(let org)): return "org \(org)"
            case (.none, .none): return "no scope"
            }
        }
    }

    /// The identity carried by a certificate.
    struct WendyIdentity: Equatable, Sendable {
        var orgID: Int32?
        var entityType: String  // "user", "asset" or "signer"
        var entityID: String
        var tenantUUID: String?
        var principal: String?

        var scope: Scope { Scope(tenantUUID: tenantUUID, orgID: orgID) }
    }

    /// A org claim was present but malformed, ambiguous, or non-positive. A
    /// client presenting such a certificate is rejected under every enforcement
    /// mode (it is anomalous, not merely legacy).
    enum OrgIdentityError: Error, Equatable {
        case multipleOrgURNs(Int)
        case multiplePrincipals(Int)
        case invalidURN(String)
        case invalidPrincipal(String)
        case invalidCommonName(String)
        case nonPositiveOrg(Int32)
    }

    private static let wendyOrgURNPrefix = "urn:wendy:org:"
    private static let tenantSPIFFEPrefix = "spiffe://wendy.sh/tenant/"

    /// The full identity, or `nil` when the certificate carries no Wendy org
    /// identity at all. Throws `OrgIdentityError` when an org claim is present but
    /// cannot be parsed.
    static func identity(fromLeaf certificate: Certificate) throws -> WendyIdentity? {
        // 1./2. SAN URIs. Decoding the SAN extension can throw for a malformed
        // extension; treat that as "no SAN present" and fall through to the
        // CommonName, rather than failing the whole lookup.
        var principals: [String] = []
        var urns: [String] = []
        if let sans = try? certificate.extensions.subjectAlternativeNames {
            for name in sans {
                guard case .uniformResourceIdentifier(let uri) = name else { continue }
                if uri.hasPrefix(Self.tenantSPIFFEPrefix) {
                    principals.append(uri)
                } else if uri.hasPrefix(Self.wendyOrgURNPrefix) {
                    urns.append(uri)
                }
            }
        }
        if principals.count > 1 {
            throw OrgIdentityError.multiplePrincipals(principals.count)
        }
        if urns.count > 1 {
            throw OrgIdentityError.multipleOrgURNs(urns.count)
        }

        let legacy = try urns.first.map(Self.parseWendyOrgURN)

        if let principal = principals.first {
            var identity = try Self.parsePrincipal(principal)
            // The legacy URN survives only as the org it names, so a peer that
            // can still only compare orgs keeps working against this leaf — but
            // only when the two SANs agree about which entity this is. A leaf
            // whose URN names a different entity than its principal is
            // misissued; the principal is authoritative, so a contradictory
            // legacy claim contributes nothing.
            if let legacy,
                legacy.entityType == identity.entityType, legacy.entityID == identity.entityID
            {
                identity.orgID = legacy.orgID
            }
            return identity
        }
        if let legacy {
            return legacy
        }

        // 3. CommonName legacy fallback.
        for relativeName in certificate.subject {
            for attribute in relativeName where attribute.type == .RDNAttributeType.commonName {
                let cn = attribute.value.description
                if cn.hasPrefix("sh/wendy/") {
                    return try Self.parseShWendyCN(cn)
                }
            }
        }

        // 4. No identity.
        return nil
    }

    /// The tenant scope only (entity dropped), or `nil` when the cert carries no
    /// identity. Throws when a claim is present but unparseable.
    static func scope(fromLeaf certificate: Certificate) throws -> Scope? {
        guard let scope = try Self.identity(fromLeaf: certificate)?.scope, scope.isKnown else {
            return nil
        }
        return scope
    }

    /// The org id only (entity dropped), or `nil` when the cert carries no org
    /// claim. Throws when a claim is present but unparseable. Retained for
    /// callers that genuinely need the legacy int org rather than a tenant.
    static func organizationID(fromLeaf certificate: Certificate) throws -> Int32? {
        try Self.identity(fromLeaf: certificate)?.orgID
    }

    /// Parses `spiffe://wendy.sh/tenant/<uuid>/<kind>/<name>`.
    ///
    /// Every kind pki-core mints is accepted — `operator`, `service`, `device`,
    /// `signer` — because a kind this refuses is an actor no gate can see.
    /// Cloud's `service/user-<id>` and `service/asset-<id>` spellings are
    /// unwrapped to the user/asset entity types the rest of the agent compares
    /// on; a device principal's name is path-shaped and keeps its slashes.
    static func parsePrincipal(_ principal: String) throws -> WendyIdentity {
        guard principal.hasPrefix(Self.tenantSPIFFEPrefix) else {
            throw OrgIdentityError.invalidPrincipal(principal)
        }
        let rest = String(principal.dropFirst(Self.tenantSPIFFEPrefix.count))
        let segments = rest.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard segments.count >= 3 else { throw OrgIdentityError.invalidPrincipal(principal) }
        let tenant = segments[0]
        // pki-core routes by the tenant UUID and compares it canonically, so a
        // non-canonical spelling of the same tenant is a different string to
        // every downstream comparison. Reject it rather than let two spellings
        // of one tenant fail to match each other.
        guard let parsed = UUID(uuidString: tenant), parsed.uuidString.lowercased() == tenant else {
            throw OrgIdentityError.invalidPrincipal(principal)
        }
        let kind = segments[1]
        let name = segments.dropFirst(2).joined(separator: "/")
        guard !name.isEmpty else { throw OrgIdentityError.invalidPrincipal(principal) }

        var entityType: String
        var entityID = name
        switch kind {
        case "operator":
            entityType = "user"
        case "device":
            entityType = "asset"
        case "signer":
            entityType = "signer"
        case "service":
            // A service account is a machine user (AAA contract D17). Cloud
            // encodes the entity it relayed in the name; anything else is a
            // plain service account and reads as a user.
            if name.hasPrefix("asset-") {
                entityType = "asset"
                entityID = String(name.dropFirst("asset-".count))
            } else if name.hasPrefix("user-") {
                entityType = "user"
                entityID = String(name.dropFirst("user-".count))
            } else {
                entityType = "user"
            }
            guard !entityID.isEmpty else { throw OrgIdentityError.invalidPrincipal(principal) }
        default:
            throw OrgIdentityError.invalidPrincipal(principal)
        }
        return WendyIdentity(
            orgID: nil,
            entityType: entityType,
            entityID: entityID,
            tenantUUID: tenant,
            principal: principal
        )
    }

    /// The org id parsed out of a `sh/wendy/<org>/<asset>` common name, or `nil`
    /// for any other shape. Retained for callers that only have a CN string (e.g.
    /// deriving the device's own org, whose cert uses the device CN format).
    static func organizationID(fromCommonName cn: String) -> Int32? {
        guard cn.hasPrefix("sh/wendy/"), let id = try? Self.parseShWendyCN(cn) else {
            return nil
        }
        return id.orgID
    }

    /// Parses `urn:wendy:org:<org>:(user|asset):<id>`.
    private static func parseWendyOrgURN(_ uri: String) throws -> WendyIdentity {
        let parts = uri.split(separator: ":", omittingEmptySubsequences: false).map(String.init)
        guard parts.count == 6, parts[0] == "urn", parts[1] == "wendy", parts[2] == "org" else {
            throw OrgIdentityError.invalidURN(uri)
        }
        guard let org = Int32(parts[3]) else { throw OrgIdentityError.invalidURN(uri) }
        guard org > 0 else { throw OrgIdentityError.nonPositiveOrg(org) }
        let entityType = parts[4]
        guard entityType == "user" || entityType == "asset" else {
            throw OrgIdentityError.invalidURN(uri)
        }
        guard !parts[5].isEmpty else { throw OrgIdentityError.invalidURN(uri) }
        return WendyIdentity(
            orgID: org,
            entityType: entityType,
            entityID: parts[5],
            tenantUUID: nil,
            principal: nil
        )
    }

    /// Parses `sh/wendy/<org>/<asset>`. Caller verifies the `sh/wendy/` prefix.
    private static func parseShWendyCN(_ cn: String) throws -> WendyIdentity {
        let parts = cn.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard parts.count == 4 else { throw OrgIdentityError.invalidCommonName(cn) }
        guard let org = Int32(parts[2]) else { throw OrgIdentityError.invalidCommonName(cn) }
        guard org > 0 else { throw OrgIdentityError.nonPositiveOrg(org) }
        guard !parts[3].isEmpty else { throw OrgIdentityError.invalidCommonName(cn) }
        return WendyIdentity(
            orgID: org,
            entityType: "asset",
            entityID: parts[3],
            tenantUUID: nil,
            principal: nil
        )
    }
}
