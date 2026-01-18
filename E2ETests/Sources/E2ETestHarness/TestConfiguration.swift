import Foundation

/// Environment-based configuration for E2E tests.
///
/// Configuration can be provided via environment variables:
/// - `E2E_VM_PATH`: Path to meta-wendyos-virtual repository
/// - `E2E_VM_NAME`: Lima VM name (default: `wendyos-e2e-test`)
/// - `E2E_AGENT_HOST`: Agent hostname (default: `localhost`)
/// - `E2E_AGENT_PORT`: Agent gRPC port (default: `50051`)
/// - `E2E_AGENT_TIMEOUT`: Agent ready timeout in seconds (default: `120`)
/// - `E2E_STOP_VM_AFTER`: Stop VM after tests complete (default: `false`)
/// - `E2E_USE_EXISTING_VM`: Use existing VM without creating (default: `false`)
public struct TestConfiguration: Sendable {
    /// Path to the meta-wendyos-virtual repository
    public let vmPath: String

    /// Lima VM name
    public let vmName: String

    /// Agent hostname
    public let agentHost: String

    /// Agent gRPC port
    public let agentPort: Int

    /// Agent ready timeout in seconds
    public let agentTimeout: Int

    /// Whether to stop the VM after tests complete
    public let stopVMAfterTests: Bool

    /// Whether to use an existing VM without creating
    public let useExistingVM: Bool

    /// Default configuration with auto-detection
    public static func fromEnvironment() -> TestConfiguration {
        let vmPath = ProcessInfo.processInfo.environment["E2E_VM_PATH"]
            ?? autoDetectVMPath()
        let vmName = ProcessInfo.processInfo.environment["E2E_VM_NAME"]
            ?? "wendyos-e2e-test"
        let agentHost = ProcessInfo.processInfo.environment["E2E_AGENT_HOST"]
            ?? "localhost"
        let agentPort = Int(ProcessInfo.processInfo.environment["E2E_AGENT_PORT"] ?? "")
            ?? 50051
        let agentTimeout = Int(ProcessInfo.processInfo.environment["E2E_AGENT_TIMEOUT"] ?? "")
            ?? 120
        let stopVMAfterTests = ProcessInfo.processInfo.environment["E2E_STOP_VM_AFTER"]
            .map { $0.lowercased() == "true" || $0 == "1" } ?? false
        let useExistingVM = ProcessInfo.processInfo.environment["E2E_USE_EXISTING_VM"]
            .map { $0.lowercased() == "true" || $0 == "1" } ?? false

        return TestConfiguration(
            vmPath: vmPath,
            vmName: vmName,
            agentHost: agentHost,
            agentPort: agentPort,
            agentTimeout: agentTimeout,
            stopVMAfterTests: stopVMAfterTests,
            useExistingVM: useExistingVM
        )
    }

    /// Auto-detect the path to meta-wendyos-virtual repository
    private static func autoDetectVMPath() -> String {
        let fileManager = FileManager.default
        let currentDir = fileManager.currentDirectoryPath

        // Compute absolute paths for potential locations
        let possiblePaths = [
            // Sibling directory to wendy-agent (when running from E2ETests)
            URL(fileURLWithPath: currentDir).deletingLastPathComponent().appendingPathComponent("meta-wendyos-virtual").path,
            // Parent's sibling (when running from wendy-agent)
            URL(fileURLWithPath: currentDir).deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("meta-wendyos-virtual").path,
            // In home directory under git/wendy
            "\(NSHomeDirectory())/git/wendy/meta-wendyos-virtual",
            // In home directory directly
            "\(NSHomeDirectory())/meta-wendyos-virtual",
        ]

        for path in possiblePaths {
            let setupScript = (path as NSString).appendingPathComponent("scripts/setup-dev-vm.sh")
            if fileManager.fileExists(atPath: setupScript) {
                return path
            }
        }

        // Return a descriptive path to help with error messages
        return "(not found - set E2E_VM_PATH)"
    }

    /// Path to the setup-dev-vm.sh script
    public var setupScriptPath: String {
        (vmPath as NSString).appendingPathComponent("scripts/setup-dev-vm.sh")
    }

    /// Validates the configuration
    public func validate() throws {
        let fileManager = FileManager.default

        guard fileManager.fileExists(atPath: setupScriptPath) else {
            throw ConfigurationError.setupScriptNotFound(path: setupScriptPath)
        }

        guard agentPort > 0 && agentPort < 65536 else {
            throw ConfigurationError.invalidPort(agentPort)
        }

        guard agentTimeout > 0 else {
            throw ConfigurationError.invalidTimeout(agentTimeout)
        }
    }
}

/// Configuration validation errors
public enum ConfigurationError: Error, CustomStringConvertible {
    case setupScriptNotFound(path: String)
    case invalidPort(Int)
    case invalidTimeout(Int)

    public var description: String {
        switch self {
        case .setupScriptNotFound(let path):
            return "Setup script not found at: \(path). Set E2E_VM_PATH environment variable to the meta-wendyos-virtual directory."
        case .invalidPort(let port):
            return "Invalid agent port: \(port)"
        case .invalidTimeout(let timeout):
            return "Invalid agent timeout: \(timeout)"
        }
    }
}
