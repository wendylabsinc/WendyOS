import CLIOutput
import Foundation
import Logging
import WendyShared

/// Checks for CLI updates and notifies the user if a new version is available.
/// - Checks once per 24 hours to avoid excessive API calls
/// - Notifies user with the update command when a new version is available
/// - Doesn't re-notify for 24 hours
/// - Detects package manager used to install the CLI
enum UpdateChecker {
    private static let checkIntervalHours: Double = 24
    private static let promptCooldownHours: Double = 24
    private static let logger = Logger(label: "sh.wendy.utils.updateChecker")

    /// Check for updates if enough time has passed since the last check.
    /// This function is designed to be non-blocking and fail silently on errors.
    static func checkForUpdatesIfNeeded() async {
        // Skip update check in dev mode
        guard Version.current != "dev" else {
            return
        }

        // Skip if JSON mode is enabled (non-interactive)
        guard !JSONMode.isEnabled else {
            return
        }

        var config = getConfig()
        let state = config.updateCheck

        // Check if enough time has passed since last network check
        if !shouldCheckForUpdates(state: state) {
            // Even if we don't need to check the network, we might still need to prompt
            // if there's a known newer version
            if let latestVersion = state?.latestKnownVersion,
                isNewerVersion(latestVersion, than: Version.current),
                shouldPromptUser(state: state)
            {
                await promptForUpdate(latestVersion: latestVersion, config: &config)
            }
            return
        }

        // Perform the update check
        await performUpdateCheck(config: &config)
    }

    /// Force an update check, ignoring cooldowns.
    /// Used by the `wendy update` command.
    static func forceCheck() async {
        // Skip update check in dev mode
        guard Version.current != "dev" else {
            cliOutput.info("Running in development mode - skipping update check")
            return
        }

        var config = getConfig()
        await performUpdateCheck(config: &config, forcePrompt: true)
    }

    /// Check if we should perform a network check for updates
    private static func shouldCheckForUpdates(state: UpdateCheckState?) -> Bool {
        guard let state = state, let lastCheck = state.lastCheckTime else {
            return true  // Never checked before
        }
        let hoursSinceCheck = Date().timeIntervalSince(lastCheck) / 3600
        return hoursSinceCheck >= checkIntervalHours
    }

    /// Check if we should prompt the user
    private static func shouldPromptUser(state: UpdateCheckState?) -> Bool {
        guard let state = state, let lastPrompt = state.lastPromptTime else {
            return true  // Never prompted
        }
        let hoursSincePrompt = Date().timeIntervalSince(lastPrompt) / 3600
        return hoursSincePrompt >= promptCooldownHours
    }

    private static func performUpdateCheck(config: inout Config, forcePrompt: Bool = false) async {
        do {
            logger.debug("Checking for updates...")

            let releases = try await fetchReleases()

            // Find the latest stable release
            guard let latestRelease = releases.first(where: { !$0.prerelease }) else {
                logger.debug("No stable releases found")
                updateCheckTime(config: &config)
                return
            }

            // Parse version from release name (e.g., "v1.2.3" -> "1.2.3")
            let latestVersion =
                latestRelease.name.hasPrefix("v")
                ? String(latestRelease.name.dropFirst())
                : latestRelease.name

            // Update the state with latest known version
            var state = config.updateCheck ?? UpdateCheckState()
            state.lastCheckTime = Date()
            state.latestKnownVersion = latestVersion
            config.updateCheck = state

            // Compare versions
            if isNewerVersion(latestVersion, than: Version.current) {
                // Should we prompt the user?
                if forcePrompt || shouldPromptUser(state: config.updateCheck) {
                    await promptForUpdate(latestVersion: latestVersion, config: &config)
                } else {
                    logger.debug(
                        "Skipping prompt - user was recently prompted",
                        metadata: [
                            "current": "\(Version.current)",
                            "latest": "\(latestVersion)",
                        ]
                    )
                    try? config.save()
                }
            } else {
                logger.debug(
                    "CLI is up to date",
                    metadata: [
                        "current": "\(Version.current)",
                        "latest": "\(latestVersion)",
                    ]
                )
                if forcePrompt {
                    cliOutput.success("You're up to date! (v\(Version.current))")
                }
                try? config.save()
            }
        } catch {
            // Fail silently - update checks should never block the user
            logger.debug(
                "Update check failed",
                metadata: ["error": "\(error)"]
            )
            if forcePrompt {
                cliOutput.warning("Could not check for updates: \(error.localizedDescription)")
            }
        }
    }

    private static func updateCheckTime(config: inout Config) {
        var state = config.updateCheck ?? UpdateCheckState()
        state.lastCheckTime = Date()
        config.updateCheck = state
        try? config.save()
    }

    private static func promptForUpdate(latestVersion: String, config: inout Config) async {
        // Display the update notification
        cliOutput.info(
            """

            A new version of wendy is available!
            Current: v\(Version.current)
            Latest:  v\(latestVersion)
            """
        )

        // Update the state with notification time
        var state = config.updateCheck ?? UpdateCheckState()
        state.lastPromptTime = Date()
        state.lastPromptResponse = nil
        config.updateCheck = state

        // Detect package manager (use cached value if available)
        let packageManager: PackageManagerType
        if let cached = state.detectedPackageManager {
            packageManager = cached
        } else {
            packageManager = await PackageManagerDetector.detect()
            state.detectedPackageManager = packageManager
            config.updateCheck = state
        }

        let command = PackageManagerDetector.updateCommand(for: packageManager)

        cliOutput.success(
            """
            Run this command to update:
              \(command)
            """
        )

        try? config.save()
    }

    /// Compare semantic versions. Returns true if `new` is greater than `current`.
    static func isNewerVersion(_ new: String, than current: String) -> Bool {
        let newComponents = new.split(separator: ".").compactMap { Int($0) }
        let currentComponents = current.split(separator: ".").compactMap { Int($0) }

        // Pad arrays to same length
        let maxLength = max(newComponents.count, currentComponents.count)
        let paddedNew = newComponents + Array(repeating: 0, count: maxLength - newComponents.count)
        let paddedCurrent =
            currentComponents + Array(repeating: 0, count: maxLength - currentComponents.count)

        for (n, c) in zip(paddedNew, paddedCurrent) {
            if n > c { return true }
            if n < c { return false }
        }

        return false  // Versions are equal
    }
}
