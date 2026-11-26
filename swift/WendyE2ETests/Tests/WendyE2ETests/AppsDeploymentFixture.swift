import Foundation
import Testing
import WendyE2ETesting

// Each tier requires an explicit opt-in; run evidence is recorded separately.
let appsDeploymentTestsEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEPLOYMENTS"] == "true"
let appsDeploymentLocalTestsEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEPLOYMENTS_LOCAL"] == "true"

/// Real CLI, registry delivery and HTTP observations. No backing service is replaced.
/// Uses the in-tree generated-client adapter and independent HTTP responses.
struct AppsDeploymentFixture: Sendable {
    enum Requirement: Sendable, Equatable { case runtime, cloudTarget, catalog }

    let requirement: Requirement
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
        portNumber: Int? = nil,
        requirement: Requirement = .runtime
    ) throws {
        self.requirement = requirement
        app = appID ?? "sh.wendy.e2e.apps-" + UUID().uuidString.lowercased()
        attempt = attemptID ?? UUID().uuidString.lowercased()
        name = deploymentName
        port = portNumber ?? Int.random(in: 30_000...49_999)
        try #require(appsDeploymentTestsEnabled, "Explicit Apps & Deployments opt-in required")
        try #require(WendyE2EEnvironment.isolation == .perTest, "Use per-test isolation")
        try #require(
            !WendyE2EEnvironment.parallel,
            "Use --no-parallel for the dedicated device fixture"
        )
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
        if requirement == .runtime {
            device = try HTTPAppFixture.requireLANDevice(
                deviceAddress ?? WendyE2EEnvironment.deviceAddress
            )
        } else {
            // Empty means absent, never a fabricated route. Cloud-only
            // commands do not receive a --device-address or LAN pin.
            device = deviceAddress ?? ""
        }
        if requirement == .catalog {
            asset = assetID ?? ""
        } else {
            asset = try assetID ?? Self.setting("WENDY_E2E_APPS_ASSET_UUID")
        }
        tenant = try Self.setting("WENDY_E2E_APPS_TENANT_UUID")
        cloud = try Self.setting("WENDY_E2E_APPS_CLOUD_GRPC")
        try #require(UUID(uuidString: tenant) != nil)
        if requirement != .catalog { try #require(UUID(uuidString: asset) != nil) }
        try #require(
            cloud.contains(".dev.") && cloud.hasSuffix(":443"),
            "Explicit Dev endpoint required"
        )
        if env["WENDY_E2E_APPS_ACCOUNT"] == nil {
            _ = try Self.setting("WENDY_E2E_CLI_AUTH_CONFIG_PATH")
        }
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
            // Confirm the real signed-in list before creating a project or any
            // Cloud resource. Redirects and partially loaded lists must fail here.
            if appsBrowserTestsEnabled {
                _ = try await fixture.console(cli, path: "/apps")
            }
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

    /// Records Cloud acceptance without requiring a live device or claiming
    /// runtime convergence. Targeted commands still use the normal CLI's
    /// recipient-key verification. The full response specs keep withApp.
    static func withCloudApp(
        verifyTarget: Bool = false,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (WendyE2ESession, AppsDeploymentFixture) async throws -> Void
    ) async throws {
        let fixture = try Self(requirement: verifyTarget ? .cloudTarget : .catalog)
        guard FileManager.default.isExecutableFile(atPath: observerPath) else {
            throw Prerequisite(description: "Build the in-tree public API fixture adapter")
        }
        try fixture.requireDedicatedIdentityFixture(requireDevicePin: false)
        try await CLIAndAgentScenario().run(filePath: filePath, function: function, line: line) {
            cli,
            _ in
            try await fixture.verifyCloudAccount(cli)
            if verifyTarget { try await fixture.verifyCloudTarget(cli) }
            try await fixture.writeProject(cli)
            let ledger =
                "app=\(fixture.app)\ntenant=\(fixture.tenant)\n"
                + (fixture.asset.isEmpty ? "" : "asset=\(fixture.asset)\n")
                + "deployment=\(fixture.deployment)\nattempt=\(fixture.attempt)\nobserves-runtime=false\n"
            try await cli.sh("printf '%s' \(quote(ledger)) > owned-resources.txt")
            var primaryError: (any Error)?
            do { try await body(cli, fixture) } catch { primaryError = error }
            do { try await fixture.cleanUpCatalog(cli) } catch {
                Issue.record("Owned Cloud app cleanup outstanding: \(fixture.app)")
                primaryError = primaryError ?? error
            }
            if let primaryError { throw primaryError }
        }
    }

    func verifyCloudAccount(_ cli: WendyE2ESession) async throws {
        struct Status: Decodable {
            struct Session: Decodable {
                struct Certificate: Decodable { let expired: Bool }
                let current: Bool?
                let context: String
                let cloudGrpc: String
                let principalUri: String?
                let certificate: Certificate?
            }
            let loggedIn: Bool
            let sessions: [Session]
        }
        let result = try await succeed(cli, "wendy --json auth status")
        let sanitized = Prerequisite(
            description: "Select a fresh native account in the intended Dev tenant"
        )
        // Keep all credential/status objects out of macro operand diagnostics.
        guard let status = try? JSONDecoder().decode(Status.self, from: Data(result.stdout.utf8)),
            status.loggedIn
        else { throw sanitized }
        let selected = status.sessions.filter { $0.current == true }
        guard selected.count == 1, let session = selected.first,
            session.cloudGrpc == cloud, session.certificate?.expired == false,
            session.principalUri?.hasPrefix("spiffe://wendy.sh/tenant/\(tenant)/operator/") == true,
            ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ACCOUNT"].map({
                $0 == session.context
            }) ?? true
        else { throw sanitized }
        // This refresh and self-permission observation are ordinary public
        // operations. No online-device list is required for Cloud-only work.
        _ = try await succeed(cli, "wendy --json cloud discover --all \(cloudFlag)")
        struct Profile: Decodable { let operatorId: String }
        let profile = try await observe(cli, "operator-profile", as: Profile.self)
        try #require(!profile.operatorId.isEmpty)
    }

    func verifyCloudTarget(_ cli: WendyE2ESession) async throws {
        struct Target: Decodable {
            let id: String
            let organizationId: String
            let deviceName: String
            let isComputeDevice: Bool
        }
        let target = try await observe(cli, "target", as: Target.self)
        try #require(target.id == asset && target.organizationId == tenant)
        try #require(target.isComputeDevice && target.deviceName == asset)
        // A catalog-reserved device name alone is not enrollment proof. The
        // normal deploy/rotate CLI verifies the actual recipient key itself.
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
    ) throws -> String {
        guard !target || !asset.isEmpty else {
            throw Prerequisite(
                description: "A targeted Cloud command requires an explicitly verified asset"
            )
        }
        return
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
        guard requirement == .runtime else {
            throw Prerequisite(description: "Cloud-only fixtures do not observe device inventory")
        }
        let result = try await succeed(cli, "\(deviceCommand) list")
        return try JSONDecoder().decode([App].self, from: Data(result.stdout.utf8))
    }

    func response(_ cli: WendyE2ESession) async throws -> Response? {
        guard requirement == .runtime else {
            throw Prerequisite(
                description: "Cloud-only fixtures do not observe device HTTP responses"
            )
        }
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

    func requireDedicatedIdentityFixture(requireDevicePin: Bool = true) throws {
        // Never use Testing macros on credential objects: operand capture can
        // disclose the entire auth dictionary. Sanitize parsing errors as well.
        let sanitized = Prerequisite(
            description:
                "Dedicated identity fixture is missing, unreadable or does not match the intended Dev account. Supply one fresh verified session for the required tenant and endpoint; credential details are suppressed."
        )
        if let account = ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ACCOUNT"] {
            guard !account.isEmpty,
                let path = ProcessInfo.processInfo.environment["WENDY_E2E_SAVED_CONFIG_DIR"],
                path.hasPrefix("/"),
                FileManager.default.fileExists(atPath: path + "/config.json")
            else { throw sanitized }
            // Actual account, endpoint and tenant are verified through the
            // normal CLI status/selection APIs below, without parsing secrets.
            return
        }
        do {
            let path = try Self.setting("WENDY_E2E_CLI_AUTH_CONFIG_PATH")
            let data = try Data(contentsOf: URL(fileURLWithPath: path))
            guard let config = try JSONSerialization.jsonObject(with: data) as? [String: Any],
                let sessions = config["auth"] as? [[String: Any]], sessions.count == 1,
                sessions[0]["cloudGRPC"] as? String == cloud,
                let certificates = sessions[0]["certificates"] as? [[String: Any]],
                certificates.count == 1,
                let principal = certificates[0]["principalUri"] as? String,
                principal.hasPrefix("spiffe://wendy.sh/tenant/\(tenant)/operator/")
            else { throw sanitized }
            guard requireDevicePin else { return }
            guard let pins = config["devicePins"] as? [String: [String: Any]] else {
                throw sanitized
            }
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
        let allSessions = try #require(auth["sessions"] as? [[String: Any]])
        let sessions: [[String: Any]]
        if let account = ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ACCOUNT"] {
            sessions = allSessions.filter {
                $0["context"] as? String == account && $0["current"] as? Bool == true
            }
        } else {
            sessions = allSessions
        }
        try #require(sessions.count == 1, "Exactly the intended saved account must be selected")
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
        let kind = (info["deviceType"] as? String ?? "").lowercased()
        if let path = ProcessInfo.processInfo.environment["WENDY_E2E_APPS_DEVICES_PATH"] {
            let declared = try JSONDecoder().decode(
                [AppsFleetDevice].self,
                from: Data(contentsOf: URL(fileURLWithPath: path))
            )
            let target = try #require(declared.first { $0.asset == asset && $0.host == device })
            let architecture = try #require(target.architecture)
            let reported = info["cpuArchitecture"] as? String ?? ""
            let normalize: (String) -> String = {
                $0 == "aarch64" ? "arm64" : ($0 == "amd64" ? "x86_64" : $0)
            }
            try #require(
                normalize(reported) == normalize(architecture),
                "Declared device architecture must match"
            )
            if let deviceType = target.deviceType {
                try #require(
                    kind.contains(deviceType.lowercased()),
                    "Declared device type must match"
                )
            } else {
                // Desktop Ubuntu has no WendyOS board model. Its signed asset
                // identity, Ubuntu OS, x86 architecture and hostname remain
                // mandatory; absence of a board model does not waive those.
                try #require(info["os"] as? String == "ubuntu")
                try #require(normalize(reported) == "x86_64")
                struct Profile: Decodable {
                    let hostname: String
                    let principal: String
                }
                let profile = try await observe(cli, "device-profile", as: Profile.self)
                #expect(profile.principal == "spiffe://wendy.sh/tenant/\(tenant)/device/\(asset)")
                let expectedHostname = try #require(
                    target.hostname ?? targets[0]["name"] as? String
                )
                try #require(!expectedHostname.isEmpty && profile.hostname == expectedHostname)
            }
        } else {
            try #require(["arm64", "aarch64"].contains(info["cpuArchitecture"] as? String ?? ""))
            try #require(
                kind.contains("raspberry") || kind.contains("rpi"),
                "Supply the approved device resource file for a non-Pi target"
            )
        }
    }

    func cleanUpCatalog(_ cli: WendyE2ESession) async throws {
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
        let catalog = try await catalog(cli)
        try #require(catalog.appCode == "NotFound" && catalog.app == nil)
        #expect(catalog.releases?.isEmpty == true)
        #expect(catalog.deployments?.isEmpty == true)
    }

    func cleanUp(_ cli: WendyE2ESession) async throws {
        try await cleanUpCatalog(cli)
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
