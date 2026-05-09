import Foundation
import Subprocess
import Testing
import WendyE2ETesting

/// Shows information reported by a Wendy agent.
///
/// Synopsis:
///
/// `wendy [--device DEVICE] device info [--check-updates] [--prerelease]`
///
/// `wendy --json [--device DEVICE] device info [--check-updates] [--prerelease]`
///
/// `wendy device info` has two modes:
///
/// - Interactive mode, used in a terminal when JSON output is not active.
/// - Non-interactive JSON mode, used with `--json` or when no interactive terminal is available.
///
/// Options:
///
/// - `--device DEVICE`: Connects to a specific device instead of using the default device or picker.
/// - `--json`: Emits JSON output and disables interactive prompts.
/// - `--check-updates`: Checks whether a newer agent version is available.
/// - `--prerelease`: Includes prerelease agent builds when checking for updates.
@Suite(.serialized)
struct `'wendy device info'` {
    var cli: Session
    let agent: Session

    init() async throws {
        self.cli = try await Session.begin(for: CLIAndAgentScenario.cli)
        self.agent = try await Session.begin(for: CLIAndAgentScenario.agent)
    }

    // MARK: - Selecting Devices

    /**
     Use this form when the target device is already known. The command connects directly to the selected device and does not open the interactive picker.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--device' selects an explicit device`() async throws {
        // TODO: implement.
    }

    /**
     When no explicit device is passed, the saved default device is the target. The command treats this as a normal selection and leaves the saved default unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses the configured default device`() async throws {
        // TODO: implement.
    }

    /**
     If no explicit or default device is available, interactive mode helps the user choose one. The picker discovers LAN, Bluetooth, and provider-backed devices.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `opens the device picker when no device is selected`() async throws {
        // TODO: implement.
    }

    /**
     A stale default device does not end the workflow. The command explains that the saved target is unreachable and returns the user to device selection.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `recovers from an unreachable default device`() async throws {
        // TODO: implement.
    }

    /**
     Cancelling the picker leaves the user's saved device configuration unchanged and produces no device information summary.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `cancels cleanly from the device picker`() async throws {
        // TODO: implement.
    }

    // MARK: - Printing Output

    /**
     The summary includes the agent version, OS, OS version, CPU architecture, and CLI version. Optional hardware fields appear when the agent reports them.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints human-readable device information`() async throws {
        // TODO: implement.
    }

    /**
     JSON mode is the automation contract. It emits one JSON object and does not use terminal UI, prompt text, or interactive update prompts.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json --device' prints JSON device information`() async throws {
        // TODO: implement.
    }

    /**
     When the CLI is not attached to an interactive terminal, `device info` behaves like `--json`: it avoids prompts and emits machine-readable output.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `non-interactive mode prints JSON device information`() async throws {
        // TODO: implement.
    }

    // MARK: - Handling Configuration Errors

    /**
     Device selection depends on the user's Wendy CLI configuration. If that configuration cannot be parsed, the command reports the configuration problem instead of opening the picker or contacting an agent.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports invalid CLI configuration before selecting a device`() async throws {
        // TODO: implement.
    }

    // MARK: - Handling Missing or Unreachable Devices

    /**
     JSON mode never opens the interactive picker. If no explicit device or default device is available, the command fails with a configuration diagnostic.
     */
    @Test
    func `'--json' reports a missing device without prompting`() async throws {
        let home = try Self.makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }

        try await Session.with(CLIAndAgentScenario.cli) { cli in
            let record = try await cli.sh(
                "HOME=\(Self.shellQuote(home.path)) CI=1 WENDY_ANALYTICS=false ./bin/wendy --json device info",
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            let standardOutput = record.standardOutput ?? ""
            let standardError = record.standardError ?? ""

            #expect(!record.terminationStatus.isSuccess)
            #expect(standardOutput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            #expect(standardError.contains("no device specified"))
            #expect(standardError.contains("--device"))
            #expect(standardError.contains("wendy device set-default"))
            #expect(!standardError.contains("Select a device"))
            #expect(!standardError.contains("device picker"))
        }
    }

    /**
     An explicit `--device` value is treated as the intended target. Connection failure is reported for that device instead of falling back to discovery.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--device' reports an unreachable device`() async throws {
        // TODO: implement.
    }

    /**
     Some discovered devices do not expose the Wendy agent information API. Selecting one of those devices produces a clear unsupported-target diagnostic instead of a partial information summary.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports an unsupported selected target`() async throws {
        // TODO: implement.
    }

    // MARK: - Checking for Updates

    /**
     With `--check-updates`, the command compares the connected agent to the selected release channel and reports whether an update is available.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--check-updates' reports update status`() async throws {
        // TODO: implement.
    }

    /**
     JSON update checks add stable fields for the latest version and whether it is newer than the connected agent.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json --check-updates' includes update status fields`() async throws {
        // TODO: implement.
    }

    /**
     `--prerelease` changes the update channel used by `--check-updates` while keeping the output format unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--prerelease --check-updates' checks prerelease updates`() async throws {
        // TODO: implement.
    }

    /**
     Update checks depend on the release source being reachable and returning a valid response. If the release source fails, the command reports the update-check failure rather than inventing an update status.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--check-updates' reports update-source failure`() async throws {
        // TODO: implement.
    }

    private static func makeTemporaryHome() throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-e2e-home-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

/// Deprecated compatibility alias for `wendy device info`.
///
/// Use `wendy device info` in new scripts and documentation.
@Suite(.serialized)
struct `'wendy device version'` {
    // MARK: - Compatibility

    /**
     The deprecated command reports the same device information as `wendy device info` and directs users to the replacement command.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `aliases device info with a deprecation notice`() async throws {
        // TODO: implement.
    }

    /**
     The deprecated command keeps stdout machine-readable in JSON mode. Deprecation guidance is kept out of the JSON payload so existing scripts can continue parsing the response.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json' aliases device info without contaminating JSON output`() async throws {
        // TODO: implement.
    }
}
