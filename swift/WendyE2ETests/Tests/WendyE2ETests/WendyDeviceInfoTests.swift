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
    let scenario = CLIAndAgentScenario()

    // MARK: - Selecting Devices

    /**
     Use this form when the target device is already known. The command connects directly to the selected device and does not open the interactive picker.
     */
    @Test
    func `'--device' selects an explicit device`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                mkdir -p "$HOME/.wendy"
                printf '{"defaultDevice":"default-device-that-should-not-be-used.invalid"}\n' > "$HOME/.wendy/config.json"
                """
            )
            try await cli.sh("wendy --device ::1 device info --json") {
                terminationStatus,
                standardOutput,
                standardError in
                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("\"version\""))
                #expect(standardOutput.contains("\"os\""))
                #expect(standardOutput.contains("\"cpuArchitecture\""))
                #expect(standardOutput.contains("\"cliVersion\""))
                #expect(standardError == "")
                #expect(!standardOutput.contains("Select a device"))
                #expect(!standardError.contains("Select a device"))
                #expect(!standardError.contains("default-device-that-should-not-be-used"))
            }
        }
    }

    /**
     When no explicit device is passed, the saved default device is the target. The command treats this as a normal selection and leaves the saved default unchanged.
     */
    @Test
    func `uses the configured default device`() async throws {
        // TODO: implement.
    }

    /**
     If no explicit or default device is available, interactive mode helps the user choose one. The picker discovers LAN, Bluetooth, and provider-backed devices.
     */
    @Test
    func `opens the device picker when no device is selected`() async throws {
        // TODO: implement.
    }

    /**
     A stale default device does not end the workflow. The command explains that the saved target is unreachable and returns the user to device selection.
     */
    @Test
    func `recovers from an unreachable default device`() async throws {
        // TODO: implement.
    }

    /**
     Cancelling the picker leaves the user's saved device configuration unchanged and produces no device information summary.
     */
    @Test
    func `cancels cleanly from the device picker`() async throws {
        // TODO: implement.
    }

    // MARK: - Printing Output

    /**
     The summary includes the agent version, OS, OS version, CPU architecture, and CLI version. Optional hardware fields appear when the agent reports them.
     */
    @Test
    func `prints human-readable device information`() async throws {
        // TODO: implement.
    }

    /**
     JSON mode is the automation contract. It emits one JSON object and does not use terminal UI, prompt text, or interactive update prompts.
     */
    @Test
    func `'--json --device' prints JSON device information`() async throws {
        // TODO: implement.
    }

    /**
     When the CLI is not attached to an interactive terminal, `device info` behaves like `--json`: it avoids prompts and emits machine-readable output.
     */
    @Test
    func `non-interactive mode prints JSON device information`() async throws {
        // TODO: implement.
    }

    // MARK: - Handling Configuration Errors

    /**
     Device selection depends on the user's Wendy CLI configuration. If that configuration cannot be parsed, the command reports the configuration problem instead of opening the picker or contacting an agent.
     */
    @Test
    func `reports invalid CLI configuration before selecting a device`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                mkdir -p "$HOME/.wendy"
                printf '{ invalid json\n' > "$HOME/.wendy/config.json"
                """
            )

            try await cli.sh("wendy device info --json") {
                terminationStatus,
                standardOutput,
                standardError in
                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("parsing config"))
                #expect(standardError.contains("invalid character"))
                #expect(!standardError.contains("Select a device"))
                #expect(!standardError.contains("getting agent version"))
            }
        }
    }

    // MARK: - Handling Missing or Unreachable Devices

    /**
     JSON mode never opens the interactive picker. If no explicit device or default device is available, the command fails with a configuration diagnostic.
     */
    @Test
    func `'--json' reports a missing device without prompting`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy device info --json") {
                terminationStatus,
                standardOutput,
                standardError in
                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(
                    standardError.contains(
                        "no device specified; use --device flag or set a default"
                    )
                )
                #expect(!standardError.contains("Select a device"))
            }
        }
    }

    /**
     An explicit `--device` value is treated as the intended target. Connection failure is reported for that device instead of falling back to discovery.
     */
    @Test
    func `'--device' reports an unreachable device`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                "wendy --device definitely-not-a-wendy-device.invalid device info --json"
            ) {
                terminationStatus,
                standardOutput,
                standardError in
                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("getting agent version"))
                #expect(standardError.contains("produced zero addresses"))
                #expect(!standardError.contains("Select a device"))
            }
        }
    }

    /**
     Some discovered devices do not expose the Wendy agent information API. Selecting one of those devices produces a clear unsupported-target diagnostic instead of a partial information summary.
     */
    @Test
    func `reports an unsupported selected target`() async throws {
        // TODO: implement.
    }

    // MARK: - Checking for Updates

    /**
     With `--check-updates`, the command compares the connected agent to the selected release channel and reports whether an update is available.
     */
    @Test
    func `'--check-updates' reports update status`() async throws {
        // TODO: implement.
    }

    /**
     JSON update checks add stable fields for the latest version and whether it is newer than the connected agent.
     */
    @Test
    func `'--json --check-updates' includes update status fields`() async throws {
        // TODO: implement.
    }

    /**
     `--prerelease` changes the update channel used by `--check-updates` while keeping the output format unchanged.
     */
    @Test
    func `'--prerelease --check-updates' checks prerelease updates`() async throws {
        // TODO: implement.
    }

    /**
     Update checks depend on the release source being reachable and returning a valid response. If the release source fails, the command reports the update-check failure rather than inventing an update status.
     */
    @Test
    func `'--check-updates' reports update-source failure`() async throws {
        // TODO: implement.
    }

}

/// Deprecated compatibility alias for `wendy device info`.
///
/// Use `wendy device info` in new scripts and documentation.
@Suite(.serialized)
struct `'wendy device version'` {
    let scenario = CLIAndAgentScenario()

    // MARK: - Compatibility

    /**
     The deprecated command reports the same device information as `wendy device info` and directs users to the replacement command.
     */
    @Test
    func `aliases device info with a deprecation notice`() async throws {
        // TODO: implement.
    }

    /**
     The deprecated command keeps stdout machine-readable in JSON mode. Deprecation guidance is kept out of the JSON payload so existing scripts can continue parsing the response.
     */
    @Test
    func `'--json' aliases device info without contaminating JSON output`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy device version --json") {
                terminationStatus,
                standardOutput,
                standardError in
                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(
                    standardError.contains(
                        "no device specified; use --device flag or set a default"
                    )
                )
                #expect(!standardError.localizedCaseInsensitiveContains("deprecated"))
                #expect(!standardError.contains("Select a device"))
            }
        }
    }
}
