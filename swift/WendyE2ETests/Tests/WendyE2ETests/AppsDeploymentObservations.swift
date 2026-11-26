import Foundation
import Testing
import WendyE2ETesting

// Typed public service observations, read by the in-tree Go client adapter.
// No expected response or executable helper is accepted from the environment.
struct AppsCatalog: Decodable, Sendable {
    struct App: Decodable, Sendable {
        let id: String
        let organizationId: String
        let name: String
        let lifecyclePolicyLimit: Policy?
        struct Policy: Decodable, Sendable { let keepTagged: Int? }
    }
    struct Release: Decodable, Sendable {
        let id: String
        let appId: String
        let name: String
        let imageDigest: String?
        let defaultConfigJson: String
        let requiredConfigNames: [String]
    }
    struct Binding: Decodable, Sendable {
        struct Principal: Decodable, Sendable { let id: String }
        struct Role: Decodable, Sendable { let name: String }
        struct Scope: Decodable, Sendable {
            let type: String
            let ref: String
        }
        let principal: Principal
        let role: Role
        let scope: Scope
        let effect: String
    }
    let appCode: String
    let app: App?
    let operatorId: String?
    let releases: [Release]?
    let deployments: [AppsDeploymentSnapshot.Deployment]?
    let bindings: [Binding]?
}

struct AppsDeploymentSnapshot: Decodable, Sendable {
    struct Deployment: Decodable, Sendable {
        let id: String
        let organizationId: String
        let appId: String
        let name: String
        let appReleaseId: String
        let configJson: String
        let desiredState: String
        let revision: Int
        let removedAt: String?
    }
    struct Revision: Decodable, Equatable, Sendable {
        let revision: Int
        let appReleaseId: String
        let configJson: String
        let desiredState: String
        let authorUserId: String
        let note: String
        let createdAt: String?
        let rollbackOf: Int
        let removed: Bool
    }
    struct History: Decodable, Equatable, Sendable {
        struct Move: Decodable, Equatable, Sendable {
            let deviceId: String
            let fromDeploymentId: String
            let toDeploymentId: String
        }
        let revision: Revision
        let devices: [String]
        let moves: [Move]
    }
    struct State: Decodable, Sendable {
        struct Report: Decodable, Sendable {
            let appId: String
            let appReleaseId: String?
            let deploymentId: String?
            let desiredState: String
            let reportedState: String?
            let reportedReleaseId: String?
            let reportedAt: String?
            let manifestDigest: String
            let postboxEntryId: String
            let correlationId: String
            let outcome: String
            let statementPending: Bool
            let unassigned: Bool
            let detachState: String
        }
        let assetId: String
        let assetName: String
        let state: Report
    }
    struct Pending: Decodable, Sendable {
        let kind: String
        let device: String
        let revision: Int
    }
    let deployment: Deployment
    let history: [History]
    let states: [State]
    let pending: [Pending]

    func currentTargets() -> [String] {
        history.first { $0.revision.revision == deployment.revision }?.devices ?? []
    }
}

struct AppsArtifact: Decodable, Sendable {
    struct Manifest: Decodable, Sendable {
        struct Blob: Decodable, Sendable {
            let plaintextDigest: String
            let ciphertextDigest: String
            let plaintextSize: String
            let ciphertextSize: String
        }
        let manifestDigest: String
        let aead: String
        let blobs: [Blob]
        let contentKeyId: String
    }
    struct Stored: Decodable, Sendable {
        let appReleaseId: String
        let appId: String
        let complete: Bool
        let blobCount: Int
    }
    let manifest: Manifest
    let imageName: String
    let computedDigest: String
}
struct AppsArtifactListing: Decodable, Sendable {
    let artifacts: [AppsArtifact.Stored]
}

extension AppsDeploymentFixture {
    // Fixed repository-owned binary: build it offline or in normal preparation.
    // An env-supplied binary or action script is deliberately unsupported.
    static var observerPath: String {
        URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Build/e2e-apps-fixture").path
    }

    func observe<Result: Decodable & Sendable>(
        _ cli: WendyE2ESession,
        _ operation: String,
        arguments: String = "",
        as type: Result.Type
    ) async throws -> Result {
        let result = try await succeed(cli, observerCommand(operation, arguments: arguments))
        return try JSONDecoder().decode(type, from: Data(result.stdout.utf8))
    }

    func observerCommand(_ operation: String, arguments: String = "") -> String {
        "\(Self.quote(Self.observerPath)) --operation \(Self.quote(operation)) --cloud \(Self.quote(cloud)) "
            + "--tenant \(Self.quote(tenant)) --app \(app) --name \(Self.quote(name)) --asset \(asset) \(arguments)"
    }

    func catalog(_ cli: WendyE2ESession) async throws -> AppsCatalog {
        try await observe(cli, "catalog", as: AppsCatalog.self)
    }

    func snapshot(_ cli: WendyE2ESession) async throws -> AppsDeploymentSnapshot {
        try await observe(cli, "deployment", as: AppsDeploymentSnapshot.self)
    }

    func requireCreatedCatalog(_ cli: WendyE2ESession) async throws {
        let catalog = try await catalog(cli)
        try #require(catalog.appCode == "OK")
        let entry = try #require(catalog.app)
        #expect(entry.id == app)
        #expect(entry.organizationId == tenant)
        #expect(entry.name == "E2E operator app")
        #expect(catalog.releases?.isEmpty == true)
        #expect(catalog.deployments?.isEmpty == true)
        let bindings = try #require(catalog.bindings)
        let owner = try #require(catalog.operatorId)
        try #require(!owner.isEmpty)
        #expect(
            bindings.contains {
                $0.principal.id == owner && $0.role.name == "app-owner"
                    && $0.scope.ref == app && $0.scope.type == "BINDING_SCOPE_TYPE_APP"
                    && $0.effect == "BINDING_EFFECT_ALLOW"
            }
        )
    }

    @discardableResult
    func requireArtifact(
        _ cli: WendyE2ESession,
        release: Release,
        defaultUnit: String? = nil
    ) async throws -> AppsArtifact {
        let catalog = try await catalog(cli)
        let row = try #require(catalog.releases?.first { $0.id == release.id })
        #expect(row.appId == app)
        #expect(row.name == release.tag)
        #expect(row.imageDigest == release.digest)
        #expect(row.requiredConfigNames == ["E2E_ATTEMPT"])
        let defaults = try JSONDecoder().decode(
            [String: String].self,
            from: Data(row.defaultConfigJson.utf8)
        )
        #expect(
            defaults["DISPLAY_UNIT"]
                == (defaultUnit ?? (release.tag == "1.0.0" ? "fahrenheit" : "kelvin"))
        )
        let artifact = try await observe(
            cli,
            "artifact",
            arguments: "--release \(release.id)",
            as: AppsArtifact.self
        )
        #expect(artifact.computedDigest == release.digest)
        #expect(artifact.manifest.manifestDigest == release.digest)
        #expect(artifact.manifest.aead == "AEAD_SUITE_AES_256_GCM_CHUNKED")
        #expect(!artifact.manifest.contentKeyId.isEmpty)
        try #require(!artifact.manifest.blobs.isEmpty)
        for blob in artifact.manifest.blobs {
            #expect(blob.plaintextDigest.hasPrefix("sha256:"))
            #expect(blob.ciphertextDigest.hasPrefix("sha256:"))
            #expect(blob.plaintextDigest != blob.ciphertextDigest)
            #expect((UInt64(blob.ciphertextSize) ?? 0) > (UInt64(blob.plaintextSize) ?? UInt64.max))
        }
        return artifact
    }

    func requireArtifactListing(
        _ cli: WendyE2ESession,
        release: Release,
        blobCount: Int
    ) async throws {
        let listing = try await observe(cli, "artifacts", as: AppsArtifactListing.self)
        let stored = try #require(listing.artifacts.first { $0.appReleaseId == release.id })
        #expect(stored.appId == app)
        #expect(stored.complete)
        #expect(stored.blobCount == blobCount)
    }

    func requireHistory(
        _ cli: WendyE2ESession,
        release: Release,
        revision: Int,
        targets: [String]? = nil,
        sameDeployment: String? = nil
    ) async throws -> AppsDeploymentSnapshot {
        let snapshot = try await snapshot(cli)
        #expect(snapshot.deployment.appId == app)
        #expect(snapshot.deployment.organizationId == tenant)
        #expect(snapshot.deployment.name == name)
        #expect(snapshot.deployment.appReleaseId == release.id)
        #expect(snapshot.deployment.revision == revision)
        if let sameDeployment { #expect(snapshot.deployment.id == sameDeployment) }
        #expect(Set(snapshot.currentTargets()) == Set(targets ?? [asset]))
        #expect(snapshot.currentTargets().count == (targets ?? [asset]).count)
        #expect(Set(snapshot.history.map { $0.revision.revision }) == Set(1...revision))
        for row in snapshot.history {
            #expect(!row.revision.authorUserId.isEmpty)
            #expect(row.revision.createdAt != nil)
        }
        return snapshot
    }
}

struct AppsRPCFailure: Decodable, Sendable { let code: String }
struct AppsAssetApps: Decodable, Sendable {
    let assetApps: [AppsDeploymentSnapshot.State.Report]
}
struct AppsKeyMaterial: Decodable, Sendable {
    let keyIds: [String]
    let sealedKeyIds: [String]
}

struct AppsOperatorProfile: Decodable, Sendable {
    let operatorId: String
    let matchesExpectedEmail: Bool
    let registeredKeyCount: Int
}

extension AppsDeploymentFixture {
    func operatorProfile(
        _ cli: WendyE2ESession,
        nativeEnvironment: String = ""
    ) async throws -> AppsOperatorProfile {
        let result = try await succeed(
            cli,
            "\(nativeEnvironment) \(observerCommand("operator-profile"))"
        )
        return try JSONDecoder().decode(AppsOperatorProfile.self, from: Data(result.stdout.utf8))
    }
}

extension AppsDeploymentFixture {
    func requireCatalogDeleted(_ cli: WendyE2ESession) async throws {
        let catalog = try await catalog(cli)
        #expect(catalog.appCode == "NotFound")
        #expect(catalog.app == nil)
        #expect(catalog.releases?.isEmpty == true)
        #expect(catalog.deployments?.isEmpty == true)
    }

    func requireDetached(_ cli: WendyE2ESession, release: Release) async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(120))
        repeat {
            let apps = try await observe(cli, "asset-apps", as: AppsAssetApps.self)
            if let state = apps.assetApps.first(where: { $0.appId == app }),
                (state.deploymentId ?? "").isEmpty, state.reportedState == "RUNNING",
                state.reportedReleaseId == release.id, !state.statementPending
            {
                return
            }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description: "Device-applied direct-run report did not arrive after detach"
        )
    }

    func withRelatedApp(
        _ cli: WendyE2ESession,
        _ body: @Sendable (Self) async throws -> Void
    ) async throws {
        let other = try Self(deviceAddress: device, assetID: asset, appID: app + "-other")
        var primaryError: (any Error)?
        do {
            try await other.create(cli)
            try await body(other)
        } catch { primaryError = error }
        do { try await other.cleanUp(cli) } catch {
            Issue.record("Related catalog cleanup failed for \(other.app): \(error)")
            primaryError = primaryError ?? error
        }
        if let primaryError { throw primaryError }
    }
}

struct AppsDeclaredAsset: Decodable, Sendable { let id: String }

extension AppsDeploymentFixture {
    func requireResponse(_ cli: WendyE2ESession, version: String, unit: String) async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(120))
        repeat {
            if let r = try await response(cli), r.app == app, r.attempt == attempt,
                r.version == version, r.unit == unit
            {
                return
            }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description:
                "Independent HTTP response did not converge to the expected version/configuration"
        )
    }
}

extension AppsDeploymentFixture {
    func withUnauthorizedOperator(
        _ cli: WendyE2ESession,
        _ body: @Sendable (_ nativeEnvironment: String) async throws -> Void
    ) async throws {
        let owner = try Self.setting("WENDY_E2E_APPS_ACCOUNT")
        let other = try Self.setting("WENDY_E2E_APPS_UNAUTHORIZED_ACCOUNT")
        guard let ownerDirectory = cli.env["WENDY_CONFIG_DIR"] else {
            throw Prerequisite(description: "An explicit native owner config directory is required")
        }
        let memberDirectory =
            ProcessInfo.processInfo.environment["WENDY_E2E_APPS_MEMBER_CONFIG_DIR"]
            ?? ownerDirectory
        let nativeEnvironment =
            "env WENDY_CONFIG_DIR=\(Self.quote(memberDirectory)) WENDY_E2E_APPS_ACCOUNT=\(Self.quote(other))"
        let sanitized = Prerequisite(
            description:
                "Supply two distinct native saved Dev account references for the same tenant; no credential export is supported"
        )
        struct Status: Decodable {
            struct Session: Decodable {
                struct Certificate: Decodable { let expired: Bool }
                let context: String
                let current: Bool?
                let principalUri: String?
                let cloudGrpc: String
                let certificate: Certificate?
            }
            let sessions: [Session]
        }
        let status = try await succeed(cli, "wendy --json auth status")
        let summary = try JSONDecoder().decode(Status.self, from: Data(status.stdout.utf8))
        let memberStatus = try await succeed(cli, "\(nativeEnvironment) wendy --json auth status")
        let memberSummary = try JSONDecoder().decode(
            Status.self,
            from: Data(memberStatus.stdout.utf8)
        )
        let prefix = "spiffe://wendy.sh/tenant/\(tenant)/operator/"
        guard memberDirectory.hasPrefix("/"),
            memberDirectory != ownerDirectory || owner != other,
            let first = summary.sessions.first(where: { $0.context == owner && $0.current == true }
            ),
            let second = memberSummary.sessions.first(where: { $0.context == other }),
            let originalMemberSelector = memberSummary.sessions.first(where: { $0.current == true }
            )?.context,
            first.cloudGrpc == cloud, second.cloudGrpc == cloud,
            first.certificate?.expired == false, second.certificate?.expired == false,
            let firstURI = first.principalUri, let secondURI = second.principalUri,
            firstURI.hasPrefix(prefix), secondURI.hasPrefix(prefix), firstURI != secondURI
        else { throw sanitized }
        var primaryError: (any Error)?
        do {
            _ = try await succeed(cli, "\(nativeEnvironment) wendy auth use \(Self.quote(other))")
            let selected = try await succeed(cli, "\(nativeEnvironment) wendy --json auth status")
            let observed = try JSONDecoder().decode(Status.self, from: Data(selected.stdout.utf8))
            guard
                observed.sessions.contains(where: {
                    $0.context == other && $0.current == true && $0.principalUri == secondURI
                        && $0.cloudGrpc == cloud && $0.certificate?.expired == false
                })
            else { throw sanitized }
            _ = try await succeed(cli, "\(nativeEnvironment) wendy registry key show \(cloudFlag)")
            try await body(nativeEnvironment)
        } catch { primaryError = error }
        do {
            _ = try await succeed(
                cli,
                "\(nativeEnvironment) wendy auth use \(Self.quote(originalMemberSelector))"
            )
        } catch {
            Issue.record("Native operator selector restoration failed; stop this serial run")
            primaryError = primaryError ?? error
        }
        if let primaryError { throw primaryError }
    }
}

extension AppsDeploymentFixture {
    func requireArtifactUnavailable(_ cli: WendyE2ESession, releaseID: String) async throws {
        let result = try await command(
            cli,
            observerCommand("artifact", arguments: "--release \(releaseID)")
        )
        try #require(result.status.isFailure)
        let refusal = try JSONDecoder().decode(AppsRPCFailure.self, from: Data(result.stdout.utf8))
        #expect(
            ["NotFound", "PermissionDenied"].contains(refusal.code),
            "Missing artifact is different from a transport or credential failure"
        )
    }
}
