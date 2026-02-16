import Foundation
import Logging
import Subprocess

/// Detects the package manager used to install the CLI and provides update commands.
/// Uses binary path detection as the primary method for accuracy.
enum PackageManagerDetector {
    private static let logger = Logger(label: "sh.wendy.utils.packageManagerDetector")

    /// Detect which package manager was used to install the CLI.
    /// Uses binary path detection first, then falls back to package manager queries.
    static func detect() async -> PackageManagerType {
        // First, check the actual executable path (handles swift run case)
        let executablePath = currentExecutablePath()
        logger.debug(
            "Current executable path",
            metadata: ["path": "\(executablePath ?? "unknown")"]
        )

        // If running from a .build directory, this is a development build
        if let path = executablePath, path.contains(".build/") {
            logger.debug("Detected development build (swift run)")
            return .unknown
        }

        // Use the executable path if available, otherwise fall back to `which wendy`
        let binaryPath: String?
        if let path = executablePath, !path.isEmpty {
            binaryPath = path
        } else {
            binaryPath = await findBinaryPathViaWhich()
        }

        guard let binaryPath else {
            logger.debug("Could not determine wendy binary path")
            return .unknown
        }

        logger.debug("Detected wendy binary path", metadata: ["path": "\(binaryPath)"])

        // Determine package manager based on binary path
        return await detectFromPath(binaryPath)
    }

    /// Get the path to the currently running executable
    private static func currentExecutablePath() -> String? {
        // CommandLine.arguments[0] contains the executable path
        let arg0 = CommandLine.arguments.first
        guard let arg0, !arg0.isEmpty else { return nil }

        // Resolve to absolute path if relative
        if arg0.hasPrefix("/") {
            return arg0
        } else {
            let url = URL(fileURLWithPath: arg0).standardized
            return url.path
        }
    }

    /// Find the path to the wendy binary using `which` (fallback method)
    private static func findBinaryPathViaWhich() async -> String? {
        do {
            let result = try await Subprocess.run(
                .name("which"),
                arguments: ["wendy"],
                output: .string(limit: 1024),
                error: .discarded
            )

            guard result.terminationStatus.isSuccess,
                let output = result.standardOutput?.trimmingCharacters(in: .whitespacesAndNewlines),
                !output.isEmpty
            else {
                return nil
            }

            return output
        } catch {
            logger.debug("Failed to run 'which wendy'", metadata: ["error": "\(error)"])
            return nil
        }
    }

    /// Detect package manager based on the binary path
    private static func detectFromPath(_ path: String) async -> PackageManagerType {
        #if os(Windows)
            if await shellCommandSucceeds("winget", args: ["list", "--name", "wendy"]) {
                logger.debug("Detected package manager: winget")
                return .winget
            }

            logger.debug(
                "Could not detect package manager from Windows path",
                metadata: ["path": "\(path)"]
            )
            return .unknown
        #else
            // Homebrew on macOS (Apple Silicon)
            if path.hasPrefix("/opt/homebrew/") {
                logger.debug("Detected package manager: brew (macOS ARM)")
                return .brew
            }

            // Homebrew on macOS (Intel) or legacy Linuxbrew
            if path.hasPrefix("/usr/local/Cellar/") || path.hasPrefix("/usr/local/bin/") {
                // On macOS, /usr/local is typically Homebrew
                // On Linux, could be manual install - check if Homebrew owns it
                #if os(macOS)
                    logger.debug("Detected package manager: brew (macOS Intel)")
                    return .brew
                #else
                    // On Linux, verify it's actually Homebrew
                    if await shellCommandSucceeds("brew", args: ["list", "wendy"]) {
                        logger.debug("Detected package manager: brew (Linuxbrew)")
                        return .brew
                    }
                #endif
            }

            // Linuxbrew (common paths)
            if path.contains("linuxbrew") || path.contains(".linuxbrew") {
                logger.debug("Detected package manager: brew (Linuxbrew)")
                return .brew
            }

            // System paths - need secondary detection
            if path.hasPrefix("/usr/bin/") || path.hasPrefix("/bin/") {
                return await detectSystemPackageManager()
            }

            logger.debug(
                "Could not detect package manager from path",
                metadata: ["path": "\(path)"]
            )
            return .unknown
        #endif
    }

    /// Detect which system package manager installed wendy (for /usr/bin paths)
    private static func detectSystemPackageManager() async -> PackageManagerType {
        #if os(Linux)
            // Check apt (Debian/Ubuntu) - look for dpkg database entry
            if FileManager.default.fileExists(atPath: "/var/lib/dpkg/info/wendy.list") {
                logger.debug("Detected package manager: apt")
                return .apt
            }

            // Check pacman (Arch Linux)
            if await shellCommandSucceeds("pacman", args: ["-Q", "wendy"]) {
                logger.debug("Detected package manager: pacman")
                return .pacman
            }

            // Check rpm-based (Fedora/RHEL/CentOS)
            if await shellCommandSucceeds("rpm", args: ["-q", "wendy"]) {
                // Distinguish between dnf and yum
                if FileManager.default.fileExists(atPath: "/usr/bin/dnf") {
                    logger.debug("Detected package manager: dnf")
                    return .dnf
                } else {
                    logger.debug("Detected package manager: yum")
                    return .yum
                }
            }
        #endif

        logger.debug("Could not detect system package manager")
        return .unknown
    }

    /// Get the update command for a given package manager.
    static func updateCommand(for manager: PackageManagerType) -> String {
        switch manager {
        case .brew:
            return "brew upgrade wendylabsinc/tap/wendy"
        case .apt:
            return "sudo apt update && sudo apt upgrade wendy"
        case .pacman:
            return "sudo pacman -Syu wendy"
        case .dnf:
            return "sudo dnf upgrade wendy"
        case .yum:
            return "sudo yum update wendy"
        case .winget:
            return "winget upgrade wendy"
        case .unknown:
            return "Please update using your package manager"
        }
    }

    /// Execute a shell command and return whether it succeeded.
    private static func shellCommandSucceeds(
        _ executable: String,
        args: [String]
    ) async -> Bool {
        do {
            let result = try await Subprocess.run(
                .name(executable),
                arguments: Subprocess.Arguments(args),
                output: .discarded,
                error: .discarded
            )
            return result.terminationStatus.isSuccess
        } catch {
            logger.debug(
                "Shell command failed",
                metadata: [
                    "executable": "\(executable)",
                    "error": "\(error)",
                ]
            )
            return false
        }
    }
}
