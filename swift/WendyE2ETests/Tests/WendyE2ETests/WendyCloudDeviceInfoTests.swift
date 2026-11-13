import Foundation
import Testing
import WendyE2ETesting

/// Inspects an enrolled device through Cloud v2, without starting VMs or updates.
@Suite
struct `'wendy cloud device info'` {
    /**
     An explicit Cloud asset UUID selects the enrolled device even when a
     different default target is saved. JSON describes its agent, OS, architecture
     and enrolled tenant. Each path must match the independently configured
     device type, OS and architecture, without pinning mutable versions. The
     connection notice names the selected Cloud device on stderr; stdout remains
     one JSON object.

     This opt-in read-only check resolves the UUID in the selected Cloud endpoint,
     then queries the agent through the Cloud tunnel. A LAN hostname is not a
     substitute for an asset ID. Successful inspection does not by itself prove
     entitlement or cryptographic enforcement internals.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `reads the explicit enrolled device through Cloud`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let rows = try await CloudReferenceFixture.roster(cli, target: target, all: false)
            try CloudReferenceFixture.selected(rows, target: target)
            try #require(rows.filter { $0["name"] as? String == target.name }.count == 1)
            // Root/leaf flag positions and the unique display name select the same device.
            for command in [
                [
                    "--device", target.asset, "cloud", "device", "info", "--read-only", "--json",
                    "--cloud-grpc", target.endpoint,
                ],
                ["cloud", "device", "info", "--read-only"] + target.flags,
                [
                    "cloud", "device", "info", "--read-only", "--cloud-grpc", target.endpoint,
                    "--device", target.name, "--json",
                ],
            ] {
                let result = try await LocalCLIAssertions.run(cli, command, exit: 0)
                CloudReferenceFixture.tunnelNotice(result, target: target)
                _ = try CloudReferenceFixture.info(result, target: target)
                try await CloudReferenceFixture.preserved(cli)
            }
        }
    }

    /**
     The selector emitted by Cloud discovery can be passed verbatim to the
     ordinary `device info` command. Its endpoint, tenant and asset select the
     Cloud tunnel even when the saved default names an unrelated LAN target.
     JSON contains the enrolled tenant and the same stable device metadata as
     the Cloud command, without a picker or a LAN fallback.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `discovery selector keeps endpoint tenant and asset through device info`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let rows = try await CloudReferenceFixture.roster(cli, target: target, all: false)
            try CloudReferenceFixture.selected(rows, target: target)
            let row = try #require(rows.first { $0["id"] as? String == target.asset })
            let selector = try #require(row["selector"] as? String)
            let cloud = try await LocalCLIAssertions.run(
                cli,
                ["cloud", "device", "info", "--read-only"] + target.flags,
                exit: 0
            )
            CloudReferenceFixture.tunnelNotice(cloud, target: target)
            let selected = try await LocalCLIAssertions.run(
                cli,
                ["device", "info", "--read-only", "--device", selector, "--json"],
                exit: 0
            )
            #expect(selected.stderr == "")
            let expected = try CloudReferenceFixture.info(cloud, target: target)
            let actual = try CloudReferenceFixture.info(selected, target: target)
            for field in ["version", "os", "osVersion", "cpuArchitecture", "deviceType"] {
                #expect(actual[field] as? String == expected[field] as? String)
            }
            try await CloudReferenceFixture.preserved(cli)
        }
    }

    /**
     A non-existent explicit Cloud selector fails instead of inspecting the
     current default. Both device-info and app-inventory
     commands emit no success output, identify the refused selector, and suggest
     Cloud discovery. Missing targets never open a device tunnel.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `refuses an unknown explicit Cloud device without falling back`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let all = try await CloudReferenceFixture.roster(cli, target: target, all: true)
            try CloudReferenceFixture.selected(all, target: target)
            let missing = "e2e-missing-" + UUID().uuidString.lowercased()
            try #require(
                !all.contains { $0["name"] as? String == missing || $0["id"] as? String == missing }
            )
            for command in [["info", "--read-only"], ["apps", "list"]] {
                let result = try await LocalCLIAssertions.run(
                    cli,
                    ["cloud", "device"] + command + [
                        "--cloud-grpc", target.endpoint, "--device", missing, "--json",
                    ],
                    exit: 4
                )
                try LocalCLIAssertions.error(result, code: "no_device", exit: 4, message: missing)
                #expect(result.readableStderr.contains("wendy cloud discover --json"))
                #expect(!result.stderr.contains("Connecting to"))
                try await CloudReferenceFixture.preserved(cli)
            }
        }
    }

    /**
     Omitting the value after `--device` is a usage error, even when a default
     target exists. It fails before authentication, Cloud discovery or LAN
     connections and leaves the isolated configuration and project unchanged.
     */
    @Test
    func `rejects a missing explicit selector value before any connection`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            let config = LocalCLIAssertions.config.replacingOccurrences(
                of: "{\"analytics\"",
                with: "{\"defaultDevice\":\"unrelated-default.invalid\",\"analytics\""
            )
            try await LocalCLIAssertions.prepare(cli, config: config)
            for command in [["info", "--read-only"], ["apps", "list"]] {
                let result = try await LocalCLIAssertions.run(
                    cli,
                    ["cloud", "device"] + command + [
                        "--cloud-grpc", "127.0.0.1:1", "--json", "--device",
                    ],
                    exit: 2,
                    monitorNetwork: true,
                    monitorSavedDefault: true
                )
                try LocalCLIAssertions.error(
                    result,
                    code: "cli_usage",
                    exit: 2,
                    message: "flag needs an argument: --device"
                )
                try await LocalCLIAssertions.unchanged(cli)
            }
        }
    }

    /**
     A malformed scoped Cloud selector cannot become a LAN address or fall back
     to a saved device. Missing endpoint, tenant, or asset UUIDs fail loudly
     before connecting. The stored selection and trust configuration are intact.
     */
    @Test
    func `rejects malformed scoped selectors without using the default`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            let config = LocalCLIAssertions.config.replacingOccurrences(
                of: "{\"analytics\"",
                with: "{\"defaultDevice\":\"unrelated-default.invalid\",\"analytics\""
            )
            try await LocalCLIAssertions.prepare(cli, config: config)
            for selector in [
                "cloud://", "cloud://127.0.0.1:1/tenant/not-a-uuid/asset/not-a-uuid",
                "cloud://127.0.0.1:1/tenant/11111111-1111-4111-8111-111111111111/asset/",
            ] {
                let result = try await LocalCLIAssertions.run(
                    cli,
                    ["device", "info", "--read-only", "--device", selector, "--json"],
                    exit: 1,
                    monitorNetwork: true,
                    monitorSavedDefault: true
                )
                try LocalCLIAssertions.error(
                    result,
                    code: "error",
                    exit: 1,
                    message: selector == "cloud://"
                        ? "invalid cloud device selector"
                        : "cloud tenant and asset IDs must be nonzero UUIDs"
                )
                #expect(!result.stderr.contains("Connecting to"))
                try await LocalCLIAssertions.unchanged(cli)
            }
        }
    }

    /**
     An unauthenticated Cloud command cannot use a saved LAN default. Explicit
     UUIDs, whitespace, and absent selectors all require Cloud login first, with
     no device tunnel, LAN connection or copied live credentials.
     */
    @Test
    func `requires Cloud credentials even with a saved LAN default`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            let config = LocalCLIAssertions.config.replacingOccurrences(
                of: "{\"analytics\"",
                with: "{\"defaultDevice\":\"unrelated-default.invalid\",\"analytics\""
            )
            try await LocalCLIAssertions.prepare(cli, config: config)
            for selector in [
                [], ["--device", "11111111-1111-4111-8111-111111111111"], ["--device", " "],
            ] {
                let result = try await LocalCLIAssertions.run(
                    cli,
                    [
                        "cloud", "device", "info", "--read-only", "--cloud-grpc", "127.0.0.1:1",
                        "--json",
                    ] + selector,
                    exit: 3,
                    monitorNetwork: true,
                    monitorSavedDefault: true
                )
                try LocalCLIAssertions.error(
                    result,
                    code: "auth_required",
                    exit: 3,
                    message: "not logged in"
                )
                #expect(result.readableStderr.contains("wendy auth login"))
                try await LocalCLIAssertions.unchanged(cli)
            }
        }
    }
}
