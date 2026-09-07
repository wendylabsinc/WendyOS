import Testing

@testable import WendyAgentCore

@Suite("OrgIdentity")
struct OrgIdentityTests {
    @Test("parses org from a well-formed common name")
    func parsesOrg() {
        #expect(OrgIdentity.organizationID(fromCommonName: "sh/wendy/7/42") == 7)
    }

    @Test("rejects malformed common names")
    func rejectsMalformed() {
        #expect(OrgIdentity.organizationID(fromCommonName: "sh/wendy/7") == nil)
        #expect(OrgIdentity.organizationID(fromCommonName: "nope") == nil)
        #expect(OrgIdentity.organizationID(fromCommonName: "sh/wendy/abc/42") == nil)
        #expect(OrgIdentity.organizationID(fromCommonName: "sh/wendy/0/42") == nil)
        #expect(OrgIdentity.organizationID(fromCommonName: "") == nil)
    }

    @Test("does not read an org from a user common name")
    func ignoresUserCommonName() {
        // A `wendy/user/<uid>` CN carries no org in the CN; the org lives in a
        // SAN URN instead. The CN-only helper must not invent one.
        #expect(OrgIdentity.organizationID(fromCommonName: "wendy/user/abc") == nil)
    }
}

@Suite("ClientCertAuthorizer.OrgEnforcementMode.parse")
struct OrgEnforcementModeParseTests {
    @Test("empty or absent defaults to grace")
    func defaultsToGrace() {
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse(nil) == (.grace, true))
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse("") == (.grace, true))
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse("   ") == (.grace, true))
    }

    @Test("recognizes off/grace/strict case-insensitively")
    func recognizesKnownValues() {
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse("off") == (.off, true))
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse("GRACE") == (.grace, true))
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse(" Strict ") == (.strict, true))
    }

    @Test("unknown values fall back to grace and report unrecognized")
    func unknownFallsBack() {
        #expect(ClientCertAuthorizer.OrgEnforcementMode.parse("lenient") == (.grace, false))
    }

    // MARK: - Tenant SPIFFE principals

    private static let tenant = "6f1b7d3c-6b7e-4a2f-9c1e-2b4a8d5e0f31"

    @Test("parses every principal kind pki-core mints")
    func parsesPrincipalKinds() throws {
        let base = "spiffe://wendy.sh/tenant/\(Self.tenant)"

        let op = try OrgIdentity.parsePrincipal("\(base)/operator/auth0|abc")
        #expect(op.entityType == "user")
        #expect(op.entityID == "auth0|abc")
        #expect(op.tenantUUID == Self.tenant)
        #expect(op.orgID == nil)

        // Cloud relays through the service-identity profile and encodes the
        // entity in the name; both spellings unwrap to the legacy entity types.
        let user = try OrgIdentity.parsePrincipal("\(base)/service/user-5")
        #expect(user.entityType == "user")
        #expect(user.entityID == "5")

        let asset = try OrgIdentity.parsePrincipal("\(base)/service/asset-42")
        #expect(asset.entityType == "asset")
        #expect(asset.entityID == "42")

        // A plain service account is a machine user (AAA contract D17).
        let sa = try OrgIdentity.parsePrincipal("\(base)/service/ci-runner")
        #expect(sa.entityType == "user")
        #expect(sa.entityID == "ci-runner")

        // A device id is path-shaped and keeps its slashes.
        let device = try OrgIdentity.parsePrincipal("\(base)/device/fleet-a/box-01")
        #expect(device.entityType == "asset")
        #expect(device.entityID == "fleet-a/box-01")

        let signer = try OrgIdentity.parsePrincipal("\(base)/signer/release")
        #expect(signer.entityType == "signer")
    }

    @Test("rejects malformed principals")
    func rejectsMalformedPrincipals() {
        let bad = [
            "spiffe://wendy.sh/tenant/not-a-uuid/operator/x",
            // Non-canonical (upper-case) UUID: pki-core compares canonically,
            // so two spellings of one tenant must not both parse.
            "spiffe://wendy.sh/tenant/\(Self.tenant.uppercased())/operator/x",
            "spiffe://wendy.sh/tenant/\(Self.tenant)/operator/",
            "spiffe://wendy.sh/tenant/\(Self.tenant)/wizard/x",
            "spiffe://wendy.sh/tenant/\(Self.tenant)",
            "spiffe://other.example/tenant/\(Self.tenant)/operator/x",
            "urn:wendy:org:7:user:5",
        ]
        for principal in bad {
            #expect(throws: (any Error).self) {
                _ = try OrgIdentity.parsePrincipal(principal)
            }
        }
    }

    @Test("scopes match only in a shared vocabulary")
    func scopeMatching() {
        let tenantA = OrgIdentity.Scope(tenantUUID: Self.tenant, orgID: nil)
        let tenantB = OrgIdentity.Scope(
            tenantUUID: "00000000-0000-4000-8000-000000000000",
            orgID: nil
        )
        let org7 = OrgIdentity.Scope(tenantUUID: nil, orgID: 7)
        let both = OrgIdentity.Scope(tenantUUID: Self.tenant, orgID: 7)

        #expect(tenantA.matches(tenantA))
        #expect(!tenantA.matches(tenantB))
        #expect(tenantA.comparable(with: tenantB))

        #expect(org7.matches(org7))
        #expect(tenantA.matches(both))

        // No shared vocabulary: unprovable, and deliberately not a match.
        #expect(!tenantA.matches(org7))
        #expect(!tenantA.comparable(with: org7))

        #expect(!OrgIdentity.Scope(tenantUUID: nil, orgID: nil).isKnown)
    }
}
