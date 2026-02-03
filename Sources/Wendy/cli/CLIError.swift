import Foundation

/// Unified error type for CLI operations.
/// Consolidates common error patterns across commands.
public enum CLIError: Error, LocalizedError, Encodable {
    // MARK: - Connection Errors

    /// Invalid endpoint format or unreachable
    case invalidEndpoint(String)

    /// No devices found during discovery
    case noDevicesFound

    /// Connection to device failed
    case connectionFailed(device: String, reason: String)

    /// Operation timed out
    case timeout(operation: String, duration: Duration)

    // MARK: - Configuration Errors

    /// Configuration file not found
    case configNotFound(path: String)

    /// Invalid configuration value
    case invalidConfig(key: String, reason: String)

    /// Failed to save configuration
    case configSaveFailed(path: String, reason: String)

    // MARK: - File System Errors

    /// Failed to create directory
    case directoryCreationFailed(path: String, reason: String)

    /// Failed to create file
    case fileCreationFailed(path: String, reason: String)

    /// File not found
    case fileNotFound(path: String)

    // MARK: - Command Execution Errors

    /// External command failed
    case commandFailed(command: String, exitCode: Int32, output: String)

    /// Missing required argument
    case missingArgument(name: String, description: String)

    /// Invalid argument value
    case invalidArgument(name: String, value: String, reason: String)

    // MARK: - Interactive Mode Errors

    /// Interactive selection not available (e.g., in JSON mode)
    case interactiveRequired(description: String)

    /// User cancelled operation
    case userCancelled

    /// Selection failed
    case selectionFailed(reason: String)

    // MARK: - Service Errors

    /// Helper service not installed
    case serviceNotInstalled(name: String)

    /// Service operation failed
    case serviceOperationFailed(service: String, operation: String, reason: String)

    /// Unsupported platform or version
    case unsupportedPlatform(reason: String)

    // MARK: - LocalizedError

    public var errorDescription: String? {
        switch self {
        // Connection
        case .invalidEndpoint(let endpoint):
            return "Invalid endpoint: \(endpoint)"
        case .noDevicesFound:
            return "No Wendy devices found"
        case .connectionFailed(let device, let reason):
            return "Connection to \(device) failed: \(reason)"
        case .timeout(let operation, let duration):
            return "\(operation) timed out after \(duration)"

        // Configuration
        case .configNotFound(let path):
            return "Configuration file not found at '\(path)'"
        case .invalidConfig(let key, let reason):
            return "Invalid configuration for '\(key)': \(reason)"
        case .configSaveFailed(let path, let reason):
            return "Failed to save configuration to '\(path)': \(reason)"

        // File System
        case .directoryCreationFailed(let path, let reason):
            return "Failed to create directory at '\(path)': \(reason)"
        case .fileCreationFailed(let path, let reason):
            return "Failed to create file at '\(path)': \(reason)"
        case .fileNotFound(let path):
            return "File not found: \(path)"

        // Command Execution
        case .commandFailed(let command, let exitCode, let output):
            return "Command '\(command)' failed with exit code \(exitCode): \(output)"
        case .missingArgument(let name, let description):
            return "Missing required argument '--\(name)': \(description)"
        case .invalidArgument(let name, let value, let reason):
            return "Invalid value '\(value)' for argument '--\(name)': \(reason)"

        // Interactive
        case .interactiveRequired(let description):
            return "Interactive selection not available in JSON mode. \(description)"
        case .userCancelled:
            return "Operation cancelled by user"
        case .selectionFailed(let reason):
            return "Selection failed: \(reason)"

        // Service
        case .serviceNotInstalled(let name):
            return "\(name) is not installed. Run the install command first."
        case .serviceOperationFailed(let service, let operation, let reason):
            return "\(service) \(operation) failed: \(reason)"
        case .unsupportedPlatform(let reason):
            return "Unsupported platform: \(reason)"
        }
    }

    /// Optional suggestion for how to resolve the error
    public var suggestion: String? {
        switch self {
        case .noDevicesFound:
            return "Make sure your device is powered on and connected to the same network"
        case .configNotFound:
            return "Run 'wendy init' to create a new project configuration"
        case .serviceNotInstalled(let name):
            return "Run 'wendy helper install' to install \(name)"
        case .interactiveRequired:
            return "Provide the selection via CLI arguments"
        case .missingArgument(let name, _):
            return "Use --\(name) to provide the required value"
        default:
            return nil
        }
    }

    /// Error code for JSON output
    public var code: String {
        switch self {
        case .invalidEndpoint: return "invalid_endpoint"
        case .noDevicesFound: return "no_devices_found"
        case .connectionFailed: return "connection_failed"
        case .timeout: return "timeout"
        case .configNotFound: return "config_not_found"
        case .invalidConfig: return "invalid_config"
        case .configSaveFailed: return "config_save_failed"
        case .directoryCreationFailed: return "directory_creation_failed"
        case .fileCreationFailed: return "file_creation_failed"
        case .fileNotFound: return "file_not_found"
        case .commandFailed: return "command_failed"
        case .missingArgument: return "missing_argument"
        case .invalidArgument: return "invalid_argument"
        case .interactiveRequired: return "interactive_required"
        case .userCancelled: return "user_cancelled"
        case .selectionFailed: return "selection_failed"
        case .serviceNotInstalled: return "service_not_installed"
        case .serviceOperationFailed: return "service_operation_failed"
        case .unsupportedPlatform: return "unsupported_platform"
        }
    }

    // MARK: - Encodable

    private enum CodingKeys: String, CodingKey {
        case error, message, suggestion
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(code, forKey: .error)
        try container.encode(errorDescription, forKey: .message)
        try container.encodeIfPresent(suggestion, forKey: .suggestion)
    }
}

// MARK: - Subprocess Error

/// Error from running an external subprocess.
/// Shared between SwiftPM and DockerCLI.
public struct SubprocessError: Error, LocalizedError, Encodable {
    public let command: String
    public let exitCode: Int
    public let output: String

    public init(command: String, exitCode: Int, output: String = "", error: String = "") {
        self.command = command
        self.exitCode = exitCode
        // Combine stdout and stderr
        if output.isEmpty {
            self.output = error
        } else if error.isEmpty {
            self.output = output
        } else {
            self.output = output + "\n" + error
        }
    }

    public var errorDescription: String? {
        if output.isEmpty {
            return "'\(command)' exited with code \(exitCode)"
        }
        return "'\(command)' exited with code \(exitCode):\n\(output)"
    }

    private enum CodingKeys: String, CodingKey {
        case error, message, exitCode, command
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("subprocess_exit", forKey: .error)
        try container.encode(errorDescription, forKey: .message)
        try container.encode(command, forKey: .command)
        try container.encode(exitCode, forKey: .exitCode)
    }
}
