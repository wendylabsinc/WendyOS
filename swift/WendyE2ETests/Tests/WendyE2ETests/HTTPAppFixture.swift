import Foundation
import Testing
import WendyE2ETesting

/// Explicit opt-in: builds and mutates only a uniquely named app on the selected device.
let httpAppTestsEnabled = ProcessInfo.processInfo.environment["WENDY_E2E_HTTP_APP"] == "true"

struct HTTPAppFixture: Sendable {
    let app = "e2e-" + UUID().uuidString.lowercased()
    let port = Int.random(in: 30_000...49_999)
    let device: String
    let mode: ProjectMode

    enum ProjectMode: Sendable { case sourceBuild, prebuiltCompose }

    struct InvalidLANDevice: Error, CustomStringConvertible {
        var description: String {
            "HTTP app fixture requires a plain dotted LAN hostname or IPv4 address; transport selectors, ports and credentials are unsupported"
        }
    }

    init(device: String, mode: ProjectMode) throws {
        self.device = try Self.requireLANDevice(device)
        self.mode = mode
    }

    static func requireLANDevice(_ device: String?) throws -> String {
        guard let device, device.utf8.count <= 253 else { throw InvalidLANDevice() }
        let alphanumeric: (UInt8) -> Bool = {
            (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0)
        }
        let labels = device.split(separator: ".", omittingEmptySubsequences: false)
        guard labels.count >= 2,
            labels.allSatisfy({ label in
                !label.isEmpty && label.utf8.count <= 63
                    && alphanumeric(label.utf8.first!) && alphanumeric(label.utf8.last!)
                    && label.utf8.allSatisfy { alphanumeric($0) || $0 == 45 }
            })
        else { throw InvalidLANDevice() }
        if device.utf8.allSatisfy({ (48...57).contains($0) || $0 == 46 }) {
            guard labels.count == 4,
                labels.allSatisfy({ Int($0).map { (0...255).contains($0) } ?? false })
            else { throw InvalidLANDevice() }
        }
        return device
    }
    // Official Python multiarch index; linux/arm64 layers total about 20 MB compressed.
    static let prebuiltImage =
        "docker.io/library/python@sha256:303398d5c9f110790bce60d64f902e51e1a061e33292985c72bf6cd07960bf09"

    var command: String {
        "env WENDY_RUN_NO_CLOUD_FALLBACK=1 wendy --json --device \(Self.quote(device))"
    }
    var buildHostArgument: String { "--build-host=\(Self.quote(device))" }
    var url: String { "http://\(device):\(port)/ready" }

    struct App: Decodable, Equatable, Sendable {
        let name: String
        let version: String?
        let runningState: String?
        let httpPort: Int?
    }

    struct ImageCleanupUnverified: Error, CustomStringConvertible {
        let app: String
        var description: String {
            "Unresolved owned image: app \(app), repository \(app):latest. Deployment may have imported an image without creating a container; app inventory cannot verify or remove that image. Exact image inspection/cleanup is required; no shared cache prune was attempted."
        }
    }

    struct Response: Decodable, Sendable {
        let app: String
        let instance: String
        let version: String
        let requests: Int
        let burstBytesPerStream: Int
    }

    struct ReadinessFailure: Decodable, Sendable {
        let status: String
        let readiness: String
    }

    struct RunResult: Decodable, Sendable {
        let app: String
        let status: String
        let readiness: String
        let url: String?
    }

    static func scenario() throws -> CLIAndAgentScenario {
        // Reject explicit transport selectors before setup can issue any command.
        _ = try requireLANDevice(WendyE2EEnvironment.deviceAddress)
        try #require(
            (ProcessInfo.processInfo.environment["WENDY_AGENT_SOCKET"] ?? "").isEmpty,
            "Unset WENDY_AGENT_SOCKET before a device test; it overrides --device"
        )
        try #require(
            WendyE2EEnvironment.isolation != .none,
            "HTTP app tests require an isolated HOME"
        )
        let script = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent(".github/ci-tests/http-app-loop/prepare_lan_auth.py").path
        return CLIAndAgentScenario(
            before: { cli, _ in
                try #require(
                    cli.machine.isLocal && cli.machine.os != .windows,
                    "LAN fixture preparation requires a local POSIX CLI host"
                )
                try await cli.sh("python3 \(Self.quote(script))")
            },
            after: { cli, _ in
                try await cli.sh(#"rm -f "$HOME/.wendy/config.json""#)
            }
        )
    }

    static func withApp(
        cli: WendyE2ESession,
        waitReady: Bool = false,
        body: @Sendable (HTTPAppFixture) async throws -> Void
    ) async throws {
        try await withProject(cli: cli) { fixture in
            try await fixture.deploy(cli, waitReady: waitReady)
            try await body(fixture)
        }
    }

    static func withComposeApp(
        cli: WendyE2ESession,
        body: @Sendable (HTTPAppFixture) async throws -> Void
    ) async throws {
        // The immutable public image is shared cache content, not an app-owned
        // image tag. Remove the exact owned app; never delete that shared image.
        try await withProject(cli: cli, imageMayBeImported: false, mode: .prebuiltCompose) {
            fixture in
            try await fixture.deploy(cli, waitReady: false)
            try await body(fixture)
        }
    }

    static func withProject(
        cli: WendyE2ESession,
        imageMayBeImported: Bool = true,
        mode: ProjectMode = .sourceBuild,
        body: @Sendable (HTTPAppFixture) async throws -> Void
    ) async throws {
        let device = try requireLANDevice(WendyE2EEnvironment.deviceAddress)
        try #require(
            cli.machine.isLocal && cli.machine.os != .windows,
            "Fixture requires a local POSIX CLI host"
        )
        let fixture = try HTTPAppFixture(device: device, mode: mode)
        let before = try await fixture.inventory(cli)
        try #require(!before.contains { $0.name == fixture.app })
        // A pre-existing listener must never be mistaken for this service's readiness.
        try await cli.sh(
            "python3 -c \(quote("import socket; s=socket.socket(); s.settimeout(2); assert s.connect_ex((\(String(reflecting: device)), \(fixture.port))) != 0, 'fixture port already in use'"))"
        )

        var primaryError: (any Error)?
        do {
            try await fixture.prepare(cli)
            try await body(fixture)
        } catch {
            primaryError = error
        }
        do {
            let remaining = try await fixture.inventory(cli)
            if remaining.contains(where: { $0.name == fixture.app }) {
                let imageCleanup = mode == .sourceBuild ? "--cleanup" : ""
                try await cli.sh(
                    "\(fixture.command) device apps remove \(quote(fixture.app)) --force \(imageCleanup)"
                )
            } else if imageMayBeImported {
                // Container inventory cannot prove that an earlier image import was undone.
                // The public CLI has no exact image-only cleanup command. Never prune
                // shared caches or pretend that an absent container proves no image leaked.
                throw ImageCleanupUnverified(app: fixture.app)
            }
            let cleaned = try await fixture.inventory(cli)
            try #require(!cleaned.contains { $0.name == fixture.app }, "Owned app must be removed")
            // Preserve the exact unrelated inventory identities through the whole journey.
            #expect(cleaned.sorted { $0.name < $1.name } == before.sorted { $0.name < $1.name })
        } catch {
            Issue.record("HTTP fixture cleanup failed for \(fixture.app): \(error)")
            primaryError = primaryError ?? error
        }
        if let primaryError { throw primaryError }
    }

    func inventory(_ cli: WendyE2ESession) async throws -> [App] {
        let result = try await cli.sh("\(command) device apps list") { result in
            try result.requireSuccess()
            #expect(result.stderr.isEmpty)
            return result
        }
        return try JSONDecoder().decode([App].self, from: Data(result.stdout.utf8))
    }

    func requireState(
        _ cli: WendyE2ESession,
        _ state: String,
        expectedVersion: String? = "1.0.0"
    ) async throws {
        let apps = try await inventory(cli).filter { $0.name == app }
        try #require(apps.count == 1, "Exactly one owned app must be listed")
        #expect(apps[0].runningState == state)
        if let expectedVersion { #expect(apps[0].version == expectedVersion) }
        #expect(apps[0].httpPort == port)
    }

    func response(
        _ cli: WendyE2ESession,
        endpoint: String? = nil,
        retryStartup: Bool = true,
        expectedVersion: String = "1.0.0",
        timeoutSeconds: Int = 2
    ) async throws -> Response {
        // Detached start acknowledges no readiness, so only that path may poll startup.
        let retry =
            retryStartup
            ? "--retry 20 --retry-delay 1 --retry-connrefused --retry-max-time 20" : "--retry 0"
        let result = try await cli.sh(
            "curl --fail --silent --show-error \(retry) --max-time \(timeoutSeconds) \(Self.quote(endpoint ?? url))"
        ) { result in
            try result.requireSuccess()
            return result
        }
        let response = try JSONDecoder().decode(Response.self, from: Data(result.stdout.utf8))
        #expect(response.app == app)
        #expect(response.version == expectedVersion)
        #expect(!response.instance.isEmpty)
        #expect(response.requests > 0)
        return response
    }

    func setVersion(_ cli: WendyE2ESession, _ version: String) async throws {
        let script = """
            import json
            from pathlib import Path
            p = Path(\(String(reflecting: app))) / 'wendy.json'
            config = json.loads(p.read_text())
            config['version'] = \(String(reflecting: version))
            config['env']['E2E_HTTP_VERSION'] = \(String(reflecting: version))
            p.write_text(json.dumps(config))
            """
        try await cli.sh("python3 -c \(Self.quote(script))")
    }

    struct LogEntry: Decodable, Sendable {
        let timestamp: String
        let severity: String
        let service: String?
        let body: String
        let attributes: [String: String]?
    }

    func logs(
        _ cli: WendyE2ESession,
        tail: Int = 20,
        level: String = "info"
    ) async throws -> [LogEntry] {
        let result = try await cli.sh(
            "\(command) device logs --app \(Self.quote(app)) --tail \(tail) --level \(level) --no-follow"
        ) { result in
            try result.requireSuccess()
            return result
        }
        return try result.stdout.split(separator: "\n").map {
            try JSONDecoder().decode(LogEntry.self, from: Data($0.utf8))
        }
    }

    func marker(_ stream: String, _ response: Response) -> String {
        "e2e \(stream) app=\(app) instance=\(response.instance) version=\(response.version) request=\(response.requests)"
    }

    func stop(_ cli: WendyE2ESession, expectedVersion: String? = "1.0.0") async throws {
        try await cli.sh("\(command) device apps stop \(Self.quote(app))") { result in
            try #require(result.status.isSuccess)
            let object =
                try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                as? [String: String]
            #expect(object?["app"] == app)
            #expect(object?["status"] == "stopped")
            #expect(object?["action"] == "stop")
        }
        try await requireState(cli, "STOPPED", expectedVersion: expectedVersion)
        try await cli.sh("curl --silent --max-time 2 \(Self.quote(url))") { result in
            #expect(result.status.isFailure, "Stopped service must not answer HTTP")
        }
    }

    func start(_ cli: WendyE2ESession) async throws {
        try await cli.sh("\(command) device apps start \(Self.quote(app)) --detach") { result in
            try #require(result.status.isSuccess)
            let object =
                try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                as? [String: String]
            #expect(object?["app"] == app)
            #expect(object?["status"] == "started")
            #expect(object?["action"] == "start")
            #expect(object?["restartPolicy"] == "unless-stopped")
        }
    }

    private func prepare(_ cli: WendyE2ESession) async throws {
        let source = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent(".github/ci-tests/http-app-loop").path
        let config: [String: Any] = [
            "appId": app, "version": "1.0.0", "language": "python",
            "env": ["E2E_HTTP_PORT": String(port)],
            "entitlements": [
                ["type": "network", "mode": "host"], ["type": "http", "port": port],
            ],
            "readiness": ["tcpSocket": ["port": port], "timeoutSeconds": 20],
        ]
        let data = try JSONSerialization.data(withJSONObject: config, options: [.sortedKeys])
        let configText = String(decoding: data, as: UTF8.self)
        try await cli.sh(
            """
            set -eu
            mkdir -p \(Self.quote(app))
            cp \(Self.quote(source))/Dockerfile \(Self.quote(source))/main.py \(Self.quote(app))/
            printf '%s' \(Self.quote(configText)) > \(Self.quote(app))/wendy.json
            """
        )
        if mode == .prebuiltCompose {
            let payload = try String(contentsOfFile: source + "/main.py", encoding: .utf8)
            let compose: [String: Any] = [
                "services": [
                    "web": [
                        "image": Self.prebuiltImage,
                        "network_mode": "host",
                        "command": ["python3", "-u", "-c", payload],
                        "restart": "no",
                    ]
                ]
            ]
            let composeData = try JSONSerialization.data(
                withJSONObject: compose,
                options: [.sortedKeys, .withoutEscapingSlashes]
            )
            try await cli.sh(
                "printf '%s' \(Self.quote(String(decoding: composeData, as: UTF8.self))) > \(Self.quote(app))/compose.yml"
            )
        }

    }

    func deploy(
        _ cli: WendyE2ESession,
        waitReady: Bool = false,
        expectedVersion: String = "1.0.0"
    ) async throws {
        let flags = waitReady ? "--wait-ready --readiness-timeout 20s" : ""
        let builder = mode == .sourceBuild ? buildHostArgument : ""
        try await cli.sh(
            "\(command) run --prefix \(Self.quote(app)) \(builder) --skip-cloud-registration --detach --no-restart \(flags)"
        ) { result in
            try #require(result.status.isSuccess)
            let output = try JSONDecoder().decode(RunResult.self, from: Data(result.stdout.utf8))
            #expect(output.app == app)
            #expect(output.status == (waitReady ? "ready" : "started"))
            #expect(output.readiness == (waitReady ? "passed" : "not_checked"))
            let endpoint = try #require(output.url)
            let components = try #require(URLComponents(string: endpoint))
            #expect(components.scheme == "http")
            #expect(components.port == port)
            if waitReady {
                // Check the declared TCP readiness condition immediately. Retrying here
                // would conceal a CLI that reports ready before the port accepts connections.
                try await cli.sh(
                    "python3 -c \(Self.quote("import socket; s=socket.socket(); s.settimeout(0.2); assert s.connect_ex((\(String(reflecting: device)), \(port))) == 0, 'CLI claimed readiness before declared TCP probe passed'"))"
                )
            }
            _ = try await response(
                cli,
                endpoint: endpoint + "/ready",
                retryStartup: !waitReady,
                expectedVersion: expectedVersion
            )
        }
    }

    private static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
