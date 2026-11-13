import Foundation
import Testing
import WendyE2ETesting

// Live tests require deliberate opt-in and an isolated, single-tenant auth file.
// Never copy a shared HOME, mint credentials, or use a LAN address as a Cloud ID.
enum CloudReferenceFixture {
    static let enabled = ProcessInfo.processInfo.environment["WENDY_E2E_CLOUD_READ_ONLY"] == "true"

    struct Target: Sendable {
        let endpoint: String
        let tenant: String
        let asset: String
        let name: String
        let pkiName: String
        let deviceType: String
        let os: String
        let architecture: String

        var selector: String { "cloud://\(endpoint)/tenant/\(tenant)/asset/\(asset)" }
        var flags: [String] { ["--cloud-grpc", endpoint, "--device", asset, "--json"] }
    }

    static func target() throws -> Target {
        func value(_ suffix: String) throws -> String {
            let key = "WENDY_E2E_CLOUD_" + suffix
            return try #require(
                ProcessInfo.processInfo.environment[key],
                "Set \(key) for the owned Cloud fixture"
            )
        }
        let target = try Target(
            endpoint: value("ENDPOINT"),
            tenant: value("TENANT_UUID"),
            asset: value("ASSET_ID"),
            name: value("DEVICE_NAME"),
            pkiName: value("PKI_DEVICE_NAME"),
            deviceType: value("DEVICE_TYPE"),
            os: value("OS"),
            architecture: value("CPU_ARCHITECTURE")
        )
        try #require(UUID(uuidString: target.tenant) != nil)
        try #require(UUID(uuidString: target.asset) != nil)
        try #require(!target.name.isEmpty && !target.pkiName.isEmpty)
        try #require(
            !target.deviceType.isEmpty && !target.os.isEmpty && !target.architecture.isEmpty
        )
        return target
    }

    static func scenario() throws -> CLIAndAgentScenario {
        try #require(
            WendyE2EEnvironment.isolation == .perTest,
            "Live credentials require per-test isolation"
        )
        return CLIAndAgentScenario(after: { cli, _ in
            // These are scenario-owned paths. Do not leave copied live credentials
            // in test homes or recorded evidence, even after a failed assertion.
            try await cli.sh(
                posix: "rm -rf \"$HOME/.wendy\" && rm -f .cloud-reference-before.json",
                power:
                    "if (Test-Path '../.wendy') { Remove-Item -Recurse -Force '../.wendy' -ErrorAction Stop }; if (Test-Path '.cloud-reference-before.json') { Remove-Item -Force '.cloud-reference-before.json' -ErrorAction Stop }"
            )
        })
    }

    static func prepare(_ cli: WendyE2ESession, target: Target) async throws {
        let home = try #require(cli.env["HOME"], "Live Cloud specs require an isolated HOME")
        try #require(cli.workingDirectory == home + (home.contains("\\") ? "\\work" : "/work"))
        // Read credentials only within the CLI sandbox, never through a recorded
        // shell result. The snapshot stays beside the isolated work directory.
        let script = """
            import datetime, json, os, pathlib, sys
            os.umask(0o077)
            endpoint, tenant = sys.argv[1:]
            path = pathlib.Path(os.environ['HOME']) / '.wendy/config.json'
            config = json.loads(path.read_text())
            entries = config.get('auth', [])
            assert len(entries) == 1, 'use a single-tenant auth fixture'
            auth = entries[0]
            assert auth['cloudGRPC'] == endpoint, 'fixture endpoint mismatch'
            principal = auth['certificates'][0]['principalUri']
            assert principal.startswith('spiffe://wendy.sh/tenant/' + tenant + '/operator/'), 'fixture tenant mismatch'
            expiry_text = auth.get('oauthExpiresAt')
            assert expiry_text, 'read-only fixture needs oauthExpiresAt freshness metadata before any CLI call'
            expiry = datetime.datetime.fromisoformat(expiry_text.replace('Z', '+00:00'))
            remaining = (expiry - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
            assert remaining > 300, 'read-only fixture needs an access token valid for at least five more minutes; obtain a new Dev sign-in, never refresh a copied rotating token'
            # A copied rotating handle can invalidate the original session. These
            # read-only scenarios retain real access credentials, never refresh authority.
            # AuthConfig uses refreshToken for both inline values and secret-store
            # references; OAuthRefreshToken() has no separate reference field.
            auth.pop('refreshToken', None)
            config['defaultDevice'] = 'unrelated-default.invalid'
            path.write_text(json.dumps(config))
            pathlib.Path('.cloud-reference-before.json').write_bytes(path.read_bytes())
            """
        try await python(cli, script, [target.endpoint, target.tenant])
    }

    static func preserved(_ cli: WendyE2ESession) async throws {
        // Cloud discovery may learn pins in this disposable HOME. It must not
        // change credentials, current context, default tenant/target or settings.
        try await python(
            cli,
            """
            import json, os, pathlib
            before = json.loads(pathlib.Path('.cloud-reference-before.json').read_text())
            after = json.loads((pathlib.Path(os.environ['HOME']) / '.wendy/config.json').read_text())
            old_pins = before.pop('devicePins', {})
            new_pins = after.pop('devicePins', {})
            assert all(new_pins.get(key) == value for key, value in old_pins.items()), 'existing trust pins changed'
            assert before == after, 'credentials or target settings changed'
            """,
            []
        )
    }

    private static func python(
        _ cli: WendyE2ESession,
        _ script: String,
        _ args: [String]
    ) async throws {
        try await cli.sh(
            posix: "python3 -I -c \(LocalCLIAssertions.quote(script)) "
                + args.map(LocalCLIAssertions.quote).joined(separator: " "),
            power: "python -I -c \(LocalCLIAssertions.powerQuote(script)) "
                + args.map(LocalCLIAssertions.powerQuote).joined(separator: " ")
                + "; if ($LASTEXITCODE -ne 0) { throw 'Cloud fixture check failed' }"
        )
    }

    static func roster(
        _ cli: WendyE2ESession,
        target: Target,
        all: Bool
    ) async throws -> [[String: Any]] {
        let args =
            ["cloud", "discover", "--cloud-grpc", target.endpoint, "--json"]
            + (all ? ["--all"] : [])
        let result = try await LocalCLIAssertions.run(cli, args, exit: 0)
        #expect(result.stderr == "")
        let rows = try #require(
            try JSONSerialization.jsonObject(with: Data(result.stdout.utf8)) as? [[String: Any]]
        )
        var ids = Set<String>()
        for row in rows {
            let id = try #require(row["id"] as? String)
            #expect(UUID(uuidString: id) != nil)
            #expect(ids.insert(id).inserted)
            #expect(row["name"] is String)
            #expect(row["type"] is String)
            #expect(row["address"] is String)
            #expect(
                row["selector"] as? String
                    == "cloud://\(target.endpoint)/tenant/\(target.tenant)/asset/\(id)"
            )
        }
        return rows
    }

    static func selected(_ rows: [[String: Any]], target: Target) throws {
        let matches = rows.filter { $0["id"] as? String == target.asset }
        try #require(matches.count == 1, "the explicit Cloud fixture asset must be present")
        let row = try #require(matches.first)
        #expect(row["name"] as? String == target.name)
        #expect(row["pkiDeviceName"] as? String == target.pkiName)
        #expect(row["selector"] as? String == target.selector)
    }

    // Fixture expectations are independent of discovery rows and sibling CLI
    // responses. JSON has no unique peer ID; these are metadata assertions,
    // not an independent proof of the identity enforced by the mTLS transport.
    static func info(_ result: WendyE2EShellResult, target: Target) throws -> [String: Any] {
        let info = try #require(
            try JSONSerialization.jsonObject(with: Data(result.stdout.utf8)) as? [String: Any]
        )
        for field in ["version", "osVersion", "cliVersion"] {
            let value = try #require(info[field] as? String)
            #expect(!value.isEmpty)
        }
        #expect(info["deviceType"] as? String == target.deviceType)
        #expect(info["os"] as? String == target.os)
        #expect(info["cpuArchitecture"] as? String == target.architecture)
        #expect(info["hasGpu"] is Bool)
        #expect(info["hasNpu"] is Bool)
        let org = try #require(info["organization"] as? [String: Any])
        #expect(org["id"] as? String == target.tenant)
        return info
    }

    static func tunnelNotice(_ result: WendyE2EShellResult, target: Target) {
        #expect(result.normalizedStderr == "Connecting to \(target.name) via cloud tunnel...\n")
        #expect(!result.stdout.contains("\u{1B}"))
    }
}
