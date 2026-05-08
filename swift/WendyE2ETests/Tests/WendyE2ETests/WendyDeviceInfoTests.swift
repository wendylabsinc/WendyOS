import Testing

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
     Selects a device explicitly with `--device`.

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
     Uses the configured default device.

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
     Opens the device picker in interactive mode.

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
     Recovers from an unreachable default device in interactive mode.

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

    // MARK: - Printing Output

    /**
     Prints human-readable device information in interactive mode.

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
     Prints JSON device information in non-interactive mode.

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
     Treats non-interactive execution as JSON mode.

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

    // MARK: - Handling Missing or Unreachable Devices

    /**
     Reports a missing device without prompting in JSON mode.

     JSON mode never opens the interactive picker. If no explicit device or default device is available, the command fails with a configuration diagnostic.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `'--json' reports a missing device without prompting`() async throws {
        // Given: no --device value and no configured default device
        // When: `wendy --json device info` is run
        // Then:
        // - exits unsuccessfully
        // - emits no JSON payload
        // - prints a clear diagnostic
        // - opens no picker
    }

    /**
     Reports an unreachable explicit device.

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

    // MARK: - Checking for Updates

    /**
     Reports agent update status.

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
     Includes update status in JSON output.

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
     Checks prerelease agent builds.

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
}

/// Deprecated compatibility alias for `wendy device info`.
///
/// Use `wendy device info` in new scripts and documentation.
@Suite(.serialized)
struct `'wendy device version'` {
    // MARK: - Compatibility

    /**
     Preserves compatibility for existing scripts.

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
}
