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
    // MARK: - Selecting Devices

    /**
     Use this form when the target device is already known. The command connects directly to the selected device and does not open the interactive picker.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--device' selects an explicit device`() async throws {
        // Given: a reachable Wendy agent address
        // When: `wendy device info --device <device>` is run
        // Then:
        // - exits successfully
        // - connects to the selected device
        // - does not open the device picker
        // - prints device information
    }

    /**
     When no explicit device is passed, the saved default device is the target. The command treats this as a normal selection and leaves the saved default unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses the configured default device`() async throws {
        // Given: a reachable default device is configured
        // When: `wendy device info` is run
        // Then:
        // - exits successfully
        // - connects to the default device
        // - does not open the picker
        // - does not rewrite the default device
    }

    /**
     If no explicit or default device is available, interactive mode helps the user choose one. The picker discovers LAN, Bluetooth, and provider-backed devices.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `opens the device picker when no device is selected`() async throws {
        // Given: no --device value and no configured default device
        // And: an interactive terminal is available
        // When: `wendy device info` is run
        // Then:
        // - opens the Bubble Tea device picker
        // - shows discovered devices
        // - connects to the selected device
        // - prints device information
    }

    /**
     A stale default device does not end the workflow. The command explains that the saved target is unreachable and returns the user to device selection.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `recovers from an unreachable default device`() async throws {
        // Given: a default device is configured but unreachable
        // And: an interactive terminal is available
        // When: `wendy device info` is run
        // Then:
        // - prints an unreachable-default warning
        // - opens the device picker
        // - allows selecting another device
    }

    /**
     Cancelling the picker leaves the user's saved device configuration unchanged and produces no device information summary.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `cancels cleanly from the device picker`() async throws {
        // Given: no --device value and no configured default device
        // And: the command opens the interactive device picker
        // When: the user cancels the picker
        // Then:
        // - exits as a user cancellation
        // - prints no device information summary
        // - does not mutate device configuration
    }

    // MARK: - Printing Output

    /**
     The summary includes the agent version, OS, OS version, CPU architecture, and CLI version. Optional hardware fields appear when the agent reports them.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints human-readable device information`() async throws {
        // Given: a selected reachable Wendy agent
        // When: `wendy device info` is run in interactive mode
        // Then:
        // - exits successfully
        // - prints agent version
        // - prints OS and OS version
        // - prints CPU architecture
        // - prints CLI version
        // - prints optional hardware details when present
    }

    /**
     JSON mode is the automation contract. It emits one JSON object and does not use terminal UI, prompt text, or interactive update prompts.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json --device' prints JSON device information`() async throws {
        // Given: a reachable Wendy agent
        // When: `wendy --json device info --device <device>` is run
        // Then:
        // - exits successfully
        // - emits one JSON object on stdout
        // - includes version, os, osVersion, cpuArchitecture, cliVersion, hasGpu
        // - includes optional hardware fields only when reported
        // - emits no prompt text
    }

    /**
     When the CLI is not attached to an interactive terminal, `device info` behaves like `--json`: it avoids prompts and emits machine-readable output.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `non-interactive mode prints JSON device information`() async throws {
        // Given: a reachable Wendy agent
        // And: no interactive terminal is available
        // When: `wendy device info --device <device>` is run
        // Then:
        // - behaves like `wendy --json device info --device <device>`
        // - emits one JSON object
        // - opens no TUI
    }

    // MARK: - Handling Configuration Errors

    /**
     Device selection depends on the user's Wendy CLI configuration. If that configuration cannot be parsed, the command reports the configuration problem instead of opening the picker or contacting an agent.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports invalid CLI configuration before selecting a device`() async throws {
        // Given: the Wendy CLI configuration file contains invalid JSON
        // When: `wendy device info --device <device>` is run
        // Then:
        // - exits unsuccessfully
        // - prints a configuration parsing diagnostic
        // - does not open the device picker
        // - does not contact the selected device
    }

    // MARK: - Handling Missing or Unreachable Devices

    /**
     JSON mode never opens the interactive picker. If no explicit device or default device is available, the command fails with a configuration diagnostic.
     */
    @Test
    func `'--json' reports a missing device without prompting`() async throws {
        // Given: no --device value and no configured default device
        let home = try Self.makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }

        // When: `wendy --json device info` is run
        try await Session.with(.cli) { cli in
            let record = try await cli.sh(
                "HOME=\(Self.shellQuote(home.path)) CI=1 WENDY_ANALYTICS=false ./bin/wendy --json device info",
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            let standardOutput = record.standardOutput ?? ""
            let standardError = record.standardError ?? ""

            // Then:
            // - exits unsuccessfully
            #expect(!record.terminationStatus.isSuccess)
            // - emits no JSON payload
            #expect(standardOutput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            // - prints a clear diagnostic
            #expect(standardError.contains("no device specified"))
            #expect(standardError.contains("--device"))
            #expect(standardError.contains("wendy device set-default"))
            // - opens no picker
            #expect(!standardError.contains("Select a device"))
            #expect(!standardError.contains("device picker"))
        }
    }

    /**
     An explicit `--device` value is treated as the intended target. Connection failure is reported for that device instead of falling back to discovery.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--device' reports an unreachable device`() async throws {
        // Given: the selected device cannot be reached
        // When: `wendy device info --device <device>` is run
        // Then:
        // - exits unsuccessfully
        // - prints a connection diagnostic
        // - opens no picker
    }

    /**
     Some discovered devices do not expose the Wendy agent information API. Selecting one of those devices produces a clear unsupported-target diagnostic instead of a partial information summary.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports an unsupported selected target`() async throws {
        // Given: the selected target does not support `wendy device info`
        // When: `wendy device info` attempts to query that target
        // Then:
        // - exits unsuccessfully
        // - prints an unsupported-target diagnostic
        // - prints no partial device information summary
    }

    // MARK: - Checking for Updates

    /**
     With `--check-updates`, the command compares the connected agent to the selected release channel and reports whether an update is available.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--check-updates' reports update status`() async throws {
        // Given: a reachable Wendy agent
        // And: the update source is available
        // When: `wendy device info --device <device> --check-updates` is run
        // Then:
        // - exits successfully
        // - prints device information
        // - reports whether an update is available
    }

    /**
     JSON update checks add stable fields for the latest version and whether it is newer than the connected agent.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json --check-updates' includes update status fields`() async throws {
        // Given: a reachable Wendy agent
        // When: `wendy --json device info --device <device> --check-updates` is run
        // Then:
        // - emits one JSON object
        // - includes latestVersion
        // - includes updateAvailable as a boolean
    }

    /**
     `--prerelease` changes the update channel used by `--check-updates` while keeping the output format unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--prerelease --check-updates' checks prerelease updates`() async throws {
        // Given: a reachable Wendy agent
        // When: `wendy device info --device <device> --check-updates --prerelease` is run
        // Then:
        // - checks the prerelease channel
        // - reports update status for that channel
    }

    /**
     Update checks depend on the release source being reachable and returning a valid response. If the release source fails, the command reports the update-check failure rather than inventing an update status.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--check-updates' reports update-source failure`() async throws {
        // Given: a reachable Wendy agent
        // And: the update source is unavailable or returns invalid data
        // When: `wendy device info --device <device> --check-updates` is run
        // Then:
        // - exits unsuccessfully
        // - prints an update-check diagnostic
        // - does not report a misleading up-to-date or update-available status
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
        // Given: a reachable Wendy agent
        // When: `wendy device version --device <device>` is run
        // Then:
        // - exits successfully
        // - prints the same semantic information as `wendy device info`
        // - reports that `wendy device version` is deprecated
        // - points to `wendy device info`
    }

    /**
     The deprecated command keeps stdout machine-readable in JSON mode. Deprecation guidance is kept out of the JSON payload so existing scripts can continue parsing the response.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json' aliases device info without contaminating JSON output`() async throws {
        // Given: a reachable Wendy agent
        // When: `wendy --json device version --device <device>` is run
        // Then:
        // - exits successfully
        // - emits the same JSON object as `wendy --json device info --device <device>`
        // - does not print deprecation text to stdout
        // - keeps any deprecation guidance outside the JSON payload
    }
}
