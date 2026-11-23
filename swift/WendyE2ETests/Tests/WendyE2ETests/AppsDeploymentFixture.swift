import Foundation
import Testing
import WendyE2ETesting

// Gates opt in to future execution; none of these drafts has been run.
let appsDeploymentTestsEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEPLOYMENTS"] == "true"
let appsDeploymentLocalTestsEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEPLOYMENTS_LOCAL"] == "true"

/// Real CLI, registry delivery and HTTP observations. No backing service is replaced.
/// Uses the in-tree generated-client adapter and independent HTTP responses.
struct AppsDeploymentFixture: Sendable {
    let app: String
    let attempt: String
    let name: String
    let port: Int
    let device: String
    let asset: String
    let tenant: String
    let cloud: String

    struct Release: Sendable {
        let id: String
        let tag: String
        let digest: String
    }

    struct App: Decodable, Equatable, Sendable {
        let name: String
        let runningState: String?
        let httpPort: Int?
    }

    struct AppliedReceipt: Codable, Sendable {
        let revision: Int
        let entry: String
    }

    struct Response: Decodable, Sendable {
        let app: String
        let attempt: String
        let version: String
        let unit: String
        let fileOnly: String
    }

    struct Prerequisite: Error, CustomStringConvertible {
        let description: String
    }

    init(
        deviceAddress: String? = nil,
        assetID: String? = nil,
        appID: String? = nil,
        attemptID: String? = nil,
        deploymentName: String = "workshop",
        portNumber: Int? = nil
    ) throws {
        app = appID ?? "sh.wendy.e2e.apps-" + UUID().uuidString.lowercased()
        attempt = attemptID ?? UUID().uuidString.lowercased()
        name = deploymentName
        port = portNumber ?? Int.random(in: 30_000...49_999)
        try #require(appsDeploymentTestsEnabled, "Explicit Apps & Deployments opt-in required")
        try #require(WendyE2EEnvironment.isolation == .perTest, "Use per-test isolation")
        try #require(!WendyE2EEnvironment.parallel, "Use --no-parallel for the one-Pi fixture")
        let env = ProcessInfo.processInfo.environment
        // Environment dictionaries can also contain credentials: keep them out
        // of assertion operand capture.
        guard env["WENDY_E2E_MANAGED_AGENT"] != "true",
            (env["WENDY_AGENT_SOCKET"] ?? "").isEmpty,
            (env["WENDY_CLOUD_INSECURE"] ?? "").isEmpty,
            (env["WENDY_REGISTRY"] ?? "").isEmpty
        else {
            throw Prerequisite(
                description:
                    "Use --no-managed-agent; unset ambient socket, insecure Cloud and registry overrides"
            )
        }
        try #require(WendyE2EMachine.cli.isLocal, "Draft fixture requires a local POSIX CLI host")
        try #require(WendyE2EMachine.cli.os != .windows, "POSIX fixture commands required")
        device = try HTTPAppFixture.requireLANDevice(
            deviceAddress ?? WendyE2EEnvironment.deviceAddress
        )
        asset = try assetID ?? Self.setting("WENDY_E2E_APPS_ASSET_UUID")
        tenant = try Self.setting("WENDY_E2E_APPS_TENANT_UUID")
        cloud = try Self.setting("WENDY_E2E_APPS_CLOUD_GRPC")
        try #require(UUID(uuidString: asset) != nil && UUID(uuidString: tenant) != nil)
        try #require(
            cloud.contains(".dev.") && cloud.hasSuffix(":443"),
            "Explicit Dev endpoint required"
        )
        _ = try Self.setting("WENDY_E2E_CLI_AUTH_CONFIG_PATH")
    }

    var deployment: String { "\(app)/\(name)" }
    var deviceCommand: String { "wendy --json --device \(Self.quote(device)) device apps" }
    var cloudFlag: String { "--cloud-grpc \(Self.quote(cloud))" }
    var url: String { "http://\(device):\(port)/" }

    static func withApp(
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (WendyE2ESession, AppsDeploymentFixture) async throws -> Void
    ) async throws {
        // All configuration checks precede scenario setup and any shell command.
        let fixture = try Self()
        guard FileManager.default.isExecutableFile(atPath: observerPath) else {
            throw Prerequisite(
                description: "Build the in-tree e2e-apps-fixture adapter before opting in"
            )
        }
        try fixture.requireDedicatedIdentityFixture()
        try await CLIAndAgentScenario().run(filePath: filePath, function: function, line: line) {
            cli,
            _ in
            try await fixture.verifyIdentity(cli)
            let before = try await fixture.inventory(cli)
            try #require(!before.contains { $0.name == fixture.app })
            // Fail before creating Cloud resources if another app owns the port.
            let probe =
                "import socket; s=socket.socket(); s.settimeout(2); "
                + "assert s.connect_ex((\(String(reflecting: fixture.device)), \(fixture.port))) != 0, 'fixture port is occupied'"
            try await cli.sh("python3 -c \(quote(probe))")
            try await fixture.writeProject(cli)
            // This ledger is public, per-test and survives a failed cleanup.
            let ledger =
                "app=\(fixture.app)\ntenant=\(fixture.tenant)\nasset=\(fixture.asset)\n"
                + "deployment=\(fixture.deployment)\nattempt=\(fixture.attempt)\nport=\(fixture.port)\n"
            try await cli.sh("printf '%s' \(quote(ledger)) > owned-resources.txt")
            var primaryError: (any Error)?
            do {
                try await body(cli, fixture)
            } catch {
                primaryError = error
            }
            do {
                try await fixture.cleanUp(cli)
                let after = try await fixture.inventory(cli)
                #expect(after.sorted { $0.name < $1.name } == before.sorted { $0.name < $1.name })
            } catch {
                Issue.record(
                    "Cleanup outstanding for app \(fixture.app), asset \(fixture.asset): \(error). See owned-resources.txt; no reset or shared-cache prune attempted."
                )
                primaryError = primaryError ?? error
            }
            if let primaryError { throw primaryError }
        }
    }

    func command(_ cli: WendyE2ESession, _ command: String) async throws -> WendyE2EShellResult {
        // Bound each CLI call, including --wait. Kill only this command's group.
        let wrapper = """
            import os, signal, subprocess, sys
            p = subprocess.Popen(['/bin/sh', '-c', sys.argv[1]], start_new_session=True)
            try:
                sys.exit(p.wait(timeout=180))
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()
                print('E2E command timed out after 180 seconds', file=sys.stderr)
                sys.exit(124)
            """
        return try await cli.sh("python3 -c \(Self.quote(wrapper)) \(Self.quote(command))") { $0 }
    }

    @discardableResult
    func succeed(_ cli: WendyE2ESession, _ text: String) async throws -> WendyE2EShellResult {
        let result = try await command(cli, text)
        try #require(result.status.isSuccess, "Command failed: \(result.readableStderr)")
        return result
    }

    func refuse(_ cli: WendyE2ESession, _ text: String, guidance: String) async throws {
        let result = try await command(cli, text + " </dev/null")
        try #require(result.status.isFailure)
        #expect((result.stdout + result.readableStderr).contains(guidance))
        #expect(!(result.stdout + result.stderr).contains("\u{1B}["))
    }

    func create(_ cli: WendyE2ESession, extra: String = "") async throws {
        _ = try await succeed(
            cli,
            "wendy app create --app \(app) --name 'E2E operator app' --org-default --yes \(cloudFlag) \(extra)"
        )
        try await requireCreatedCatalog(cli)
    }

    func writeProject(_ cli: WendyE2ESession, version: String = "1.0.0") async throws {
        let config: [String: Any] = [
            "appId": app, "version": version, "language": "python",
            "env": [
                "E2E_APP_ID": app, "E2E_ATTEMPT": "", "E2E_HTTP_PORT": String(port),
                "DISPLAY_UNIT": version == "1.0.0" ? "fahrenheit" : "kelvin",
                "FILE_ONLY": "default",
                "E2E_FAIL_ON_BOOT_ID": "",
            ],
            "requiredEnv": ["E2E_ATTEMPT"],
            "entitlements": [
                ["type": "network", "mode": "host"], ["type": "http", "port": port],
            ],
        ]
        let data = try JSONSerialization.data(withJSONObject: config, options: [.sortedKeys])
        try await cli.sh(
            "printf '%s' \(Self.quote(String(decoding: data, as: UTF8.self))) > wendy.json"
        )
    }

    func publish(
        _ cli: WendyE2ESession,
        version: String = "1.0.0",
        layoutKey: String = "WENDY_E2E_APPS_LAYOUT_A"
    ) async throws -> Release {
        let layout = try Self.setting(layoutKey)
        try await writeProject(cli, version: version)
        let result = try await succeed(
            cli,
            "wendy app publish --tag \(version) --oci-layout \(Self.quote(layout)) --platform linux/arm64 \(cloudFlag)"
        )
        // publish currently emits a human receipt rather than structured JSON.
        let text = result.stdout + "\n" + result.readableStderr
        let expression = try NSRegularExpression(
            pattern: "Published release ([0-9a-f-]+) of "
                + NSRegularExpression.escapedPattern(for: app) + ": "
                + NSRegularExpression.escapedPattern(for: version) + " = (sha256:[0-9a-f]{64})"
        )
        let match = try #require(
            expression.firstMatch(in: text, range: NSRange(text.startIndex..., in: text))
        )
        let idRange = try #require(Range(match.range(at: 1), in: text))
        let digestRange = try #require(Range(match.range(at: 2), in: text))
        let release = Release(
            id: String(text[idRange]),
            tag: version,
            digest: String(text[digestRange])
        )
        try await cli.sh(
            "printf '%s\\n' \(Self.quote("release=\(release.id) tag=\(version) digest=\(release.digest)")) >> owned-resources.txt"
        )
        try await requireArtifact(cli, release: release)
        return release
    }

    func deployCommand(
        _ release: Release,
        run: Bool = true,
        extra: String = "",
        target: Bool = true,
        confirm: Bool = true
    ) -> String {
        "wendy app deploy --app \(app) --release \(release.id) --digest \(release.digest) --name \(name) "
            + (target ? "--device \(asset) " : "")
            + "--config E2E_ATTEMPT=\(attempt) --config DISPLAY_UNIT=celsius --keep-trying 7d "
            + (run ? "--run " : "--install-only ") + (confirm ? "--yes " : "")
            + "\(cloudFlag) \(extra)"
    }

    func deployed(_ cli: WendyE2ESession) async throws -> Release {
        try await create(cli)
        let release = try await publish(cli)
        _ = try await succeed(cli, deployCommand(release))
        try await requireRunning(
            cli,
            release: release,
            revision: 1,
            version: "1.0.0",
            unit: "celsius"
        )
        _ = try await requireHistory(cli, release: release, revision: 1)
        return release
    }

    func show(_ cli: WendyE2ESession) async throws -> String {
        try await succeed(cli, "wendy app deployment show \(deployment) \(cloudFlag)").stdout
    }

    func inventory(_ cli: WendyE2ESession) async throws -> [App] {
        let result = try await succeed(cli, "\(deviceCommand) list")
        return try JSONDecoder().decode([App].self, from: Data(result.stdout.utf8))
    }

    func response(_ cli: WendyE2ESession) async throws -> Response? {
        let result = try await command(
            cli,
            "curl --silent --show-error --fail --connect-timeout 2 --max-time 3 --noproxy '*' \(Self.quote(url))"
        )
        guard result.status.isSuccess else { return nil }
        return try? JSONDecoder().decode(Response.self, from: Data(result.stdout.utf8))
    }

    func requireRunning(
        _ cli: WendyE2ESession,
        release: Release,
        revision: Int,
        version: String,
        unit: String,
        fileOnly: String = "default"
    ) async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(120))
        repeat {
            let status = try await show(cli)
            let rows = try await inventory(cli).filter { $0.name == app }
            if let response = try await response(cli),
                status.contains("release \(release.tag), run it now, revision \(revision)."),
                status.contains(" running"), rows.count == 1, rows[0].runningState == "RUNNING",
                response.app == app, response.attempt == attempt,
                response.version == version, response.unit == unit, response.fileOnly == fileOnly
            {
                let cloud = try await snapshot(cli)
                let matching = cloud.states.filter { $0.assetId == asset }
                guard cloud.deployment.appReleaseId == release.id,
                    cloud.deployment.revision == revision,
                    cloud.currentTargets().contains(asset), matching.count == 1,
                    matching[0].state.reportedReleaseId == release.id,
                    matching[0].state.manifestDigest == release.digest,
                    matching[0].state.reportedState == "RUNNING",
                    matching[0].state.outcome == "APPLY_OUTCOME_APPLIED",
                    !matching[0].state.statementPending,
                    matching[0].state.postboxEntryId != "0",
                    !matching[0].state.correlationId.isEmpty
                else {
                    try await Task.sleep(for: .seconds(2))
                    continue
                }
                let previous = try await cli.sh(
                    "cat last-applied-\(app)-\(asset).json 2>/dev/null || true"
                ) { $0.stdout }
                if let data = previous.data(using: .utf8),
                    let receipt = try? JSONDecoder().decode(AppliedReceipt.self, from: data),
                    receipt.revision != revision
                {
                    guard receipt.entry != matching[0].state.postboxEntryId else {
                        try await Task.sleep(for: .seconds(2))
                        continue
                    }
                }
                let receipt = AppliedReceipt(
                    revision: revision,
                    entry: matching[0].state.postboxEntryId
                )
                let data = try JSONEncoder().encode(receipt)
                try await cli.sh(
                    "printf '%s' \(Self.quote(String(decoding: data, as: UTF8.self))) > last-applied-\(app)-\(asset).json"
                )
                #expect(rows[0].httpPort == port)
                let config = try Self.configuration(in: status)
                #expect(config["DISPLAY_UNIT"] == unit)
                #expect(config["E2E_ATTEMPT"] == attempt)
                #expect(config["FILE_ONLY"] == fileOnly)
                return
            }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description:
                "120s rollout timeout for \(deployment), revision \(revision). Last status, inventory and HTTP responses are in command recordings. Exact report, release, digest and independent response did not converge."
        )
    }

    func requireAbsent(_ cli: WendyE2ESession) async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(120))
        repeat {
            let rows = try await inventory(cli)
            let response = try await response(cli)
            if !rows.contains(where: { $0.name == app }), response?.app != app { return }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(description: "Owned app \(app) remains on \(asset) after 120s")
    }

    static func configuration(in status: String) throws -> [String: String] {
        let line = try #require(
            status.split(separator: "\n").first { $0.hasPrefix("Configuration: ") }
        )
        return try JSONDecoder().decode(
            [String: String].self,
            from: Data(line.dropFirst("Configuration: ".count).utf8)
        )
    }

    static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    static func setting(_ name: String) throws -> String {
        guard let value = ProcessInfo.processInfo.environment[name], !value.isEmpty else {
            throw Prerequisite(
                description: "Missing prerequisite \(name); see AppsDeploymentsDraft.md"
            )
        }
        return value
    }

    func requireDedicatedIdentityFixture() throws {
        // Never use Testing macros on credential objects: operand capture can
        // disclose the entire auth dictionary. Sanitize parsing errors as well.
        let sanitized = Prerequisite(
            description:
                "Dedicated identity fixture is missing, unreadable or does not match the expected Dev tenant, endpoint and pinned asset. Supply one fresh verified session; credential details are suppressed."
        )
        do {
            let path = try Self.setting("WENDY_E2E_CLI_AUTH_CONFIG_PATH")
            let data = try Data(contentsOf: URL(fileURLWithPath: path))
            guard let config = try JSONSerialization.jsonObject(with: data) as? [String: Any],
                let sessions = config["auth"] as? [[String: Any]], sessions.count == 1,
                sessions[0]["cloudGRPC"] as? String == cloud,
                let certificates = sessions[0]["certificates"] as? [[String: Any]],
                certificates.count == 1,
                let principal = certificates[0]["principalUri"] as? String,
                principal.hasPrefix("spiffe://wendy.sh/tenant/\(tenant)/operator/"),
                let pins = config["devicePins"] as? [String: [String: Any]]
            else { throw sanitized }
            var key = device.lowercased()
            if key.hasSuffix(".") { key.removeLast() }
            if key.hasSuffix(".local") { key.removeLast(".local".count) }
            guard let pin = pins[key], pin["cloudGRPC"] as? String == cloud,
                pin["assetId"] as? String == asset,
                let devicePrincipal = pin["principal"] as? String,
                devicePrincipal.hasPrefix("spiffe://wendy.sh/tenant/\(tenant)/device/")
            else { throw sanitized }
        } catch {
            throw sanitized
        }
    }

    func verifyIdentity(_ cli: WendyE2ESession) async throws {
        let authResult = try await succeed(cli, "wendy --json auth status")
        let auth = try #require(
            try JSONSerialization.jsonObject(with: Data(authResult.stdout.utf8)) as? [String: Any]
        )
        try #require(auth["loggedIn"] as? Bool == true)
        let sessions = try #require(auth["sessions"] as? [[String: Any]])
        try #require(sessions.count == 1)
        try #require(sessions[0]["cloudGrpc"] as? String == cloud)
        let principal = try #require(sessions[0]["principalUri"] as? String)
        try #require(principal.hasPrefix("spiffe://wendy.sh/tenant/\(tenant)/operator/"))
        let cert = try #require(sessions[0]["certificate"] as? [String: Any])
        try #require(cert["expired"] as? Bool == false, "Supply fresh dedicated credentials")

        let cloudResult = try await succeed(cli, "wendy --json cloud discover \(cloudFlag)")
        let devices = try #require(
            try JSONSerialization.jsonObject(with: Data(cloudResult.stdout.utf8))
                as? [[String: Any]]
        )
        let targets = devices.filter { $0["id"] as? String == asset }
        try #require(targets.count == 1, "Intended asset must be online in this Dev session")
        try #require(
            targets[0]["selector"] as? String == "cloud://\(cloud)/tenant/\(tenant)/asset/\(asset)"
        )

        // The normal connection enforces the saved asset/principal pin above.
        let infoResult = try await succeed(
            cli,
            "wendy --json --device \(Self.quote(device)) device info --read-only"
        )
        let info = try #require(
            try JSONSerialization.jsonObject(with: Data(infoResult.stdout.utf8)) as? [String: Any]
        )
        let organization = try #require(info["organization"] as? [String: Any])
        try #require(organization["id"] as? String == tenant)
        try #require(
            info["cpuArchitecture"] as? String == "arm64"
                || info["cpuArchitecture"] as? String == "aarch64"
        )
        let kind = (info["deviceType"] as? String ?? "").lowercased()
        try #require(
            kind.contains("raspberry") || kind.contains("rpi"),
            "First journey requires the approved Pi"
        )
    }

    func cleanUp(_ cli: WendyE2ESession) async throws {
        let present = try await command(cli, "wendy app lifecycle \(app) \(cloudFlag)")
        if present.status.isSuccess {
            _ = try await succeed(cli, "wendy app delete \(app) --yes \(cloudFlag)")
        } else {
            let diagnostic = present.readableStderr.lowercased()
            try #require(
                diagnostic.contains("no app \(app) in this organization"),
                "Catalog absence could not be established"
            )
        }
        // Delete is asynchronous. Give signed removal time to apply before a
        // direct cleanup, which is needed for the deliberately detached app.
        try await requireAbsentOrRemoveDetached(cli)
        try await cli.sh(
            "printf '%s\\n' 'catalog and runtime cleanup observed; image-only cleanup unverified' >> owned-resources.txt"
        )
    }

    private func requireAbsentOrRemoveDetached(_ cli: WendyE2ESession) async throws {
        let detached = try await cli.sh("test -f detached-owned-app || test -f direct-owned-app") {
            $0.status.isSuccess
        }
        if detached {
            let rows = try await inventory(cli)
            if rows.contains(where: { $0.name == app }) {
                _ = try await succeed(cli, "\(deviceCommand) remove \(app) --force --cleanup")
            }
        }
        try await requireAbsent(cli)
    }
}
