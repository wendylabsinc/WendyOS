import Foundation
import Logging
import Noora
import WendyShared

/// Checks for CLI updates and prompts the user if a new version is available.
/// Only checks once per day to avoid excessive API calls.
enum UpdateChecker {
    private static let checkInterval: TimeInterval = 24 * 60 * 60  // 24 hours in seconds
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

        let config = getConfig()

        // Check if enough time has passed since last check
        if let lastCheck = config.lastUpdateCheck {
            let timeSinceLastCheck = Date().timeIntervalSince(lastCheck)
            guard timeSinceLastCheck >= checkInterval else {
                logger.debug(
                    "Skipping update check, last check was recent",
                    metadata: ["hours_ago": "\(Int(timeSinceLastCheck / 3600))"]
                )
                return
            }
        }

        // Perform the update check
        await performUpdateCheck()
    }

    private static func performUpdateCheck() async {
        do {
            logger.debug("Checking for updates...")

            let releases = try await fetchReleases()

            // Find the latest stable release
            guard let latestRelease = releases.first(where: { !$0.prerelease }) else {
                logger.debug("No stable releases found")
                updateLastCheckTime()
                return
            }

            // Parse version from release name (e.g., "v1.2.3" -> "1.2.3")
            let latestVersion =
                latestRelease.name.hasPrefix("v")
                ? String(latestRelease.name.dropFirst())
                : latestRelease.name

            // Compare versions
            if isNewerVersion(latestVersion, than: Version.current) {
                displayUpdatePrompt(newVersion: latestVersion)
            } else {
                logger.debug(
                    "CLI is up to date",
                    metadata: [
                        "current": "\(Version.current)",
                        "latest": "\(latestVersion)",
                    ]
                )
            }

            updateLastCheckTime()
        } catch {
            // Fail silently - update checks should never block the user
            logger.debug(
                "Update check failed",
                metadata: ["error": "\(error)"]
            )
        }
    }

    private static func updateLastCheckTime() {
        var config = getConfig()
        config.lastUpdateCheck = Date()
        try? config.save()
    }

    /// Compare semantic versions. Returns true if `new` is greater than `current`.
    private static func isNewerVersion(_ new: String, than current: String) -> Bool {
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

    private static func displayUpdatePrompt(newVersion: String) {
        Noora().info(
            """

            A new version of wendy is available: \(newVersion) (current: \(Version.current))
            Run: \(getUpdateCommand())
            """
        )
    }

    private static func getUpdateCommand() -> String {
        #if os(macOS)
        return "brew upgrade wendylabsinc/tap/wendy"
        #elseif os(Linux)
        // Detect Linux distribution and return appropriate command
        if let osRelease = try? String(contentsOfFile: "/etc/os-release", encoding: .utf8) {
            let lowercased = osRelease.lowercased()
            if lowercased.contains("debian") || lowercased.contains("ubuntu") {
                return "sudo apt-get update && sudo apt-get upgrade wendy"
            } else if lowercased.contains("fedora") {
                return "sudo dnf upgrade wendy"
            } else if lowercased.contains("rhel") || lowercased.contains("centos") || lowercased.contains("rocky") || lowercased.contains("alma") {
                return "sudo yum update wendy"
            } else if lowercased.contains("arch") {
                return "yay -Syu wendy"
            }
        }
        // Fallback: check for package manager binaries
        if FileManager.default.fileExists(atPath: "/usr/bin/apt-get") {
            return "sudo apt-get update && sudo apt-get upgrade wendy"
        } else if FileManager.default.fileExists(atPath: "/usr/bin/dnf") {
            return "sudo dnf upgrade wendy"
        } else if FileManager.default.fileExists(atPath: "/usr/bin/yum") {
            return "sudo yum update wendy"
        } else if FileManager.default.fileExists(atPath: "/usr/bin/pacman") {
            return "yay -Syu wendy"
        }
        // Generic fallback
        return "Update using your package manager"
        #else
        return "Update using your package manager"
        #endif
    }
}
