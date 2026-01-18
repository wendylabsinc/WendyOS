import Foundation
import Logging
import Subprocess

/// Manages the lifecycle of the virtual WendyOS device using setup-dev-vm.sh
public actor VMLifecycleManager {
    /// VM status states
    public enum VMStatus: Sendable, Equatable {
        case running
        case stopped
        case notFound
    }

    private let configuration: TestConfiguration
    private let logger: Logger
    private let agentClient: AgentClient

    public init(configuration: TestConfiguration, logger: Logger = Logger(label: "E2ETestHarness.VMLifecycleManager")) {
        self.configuration = configuration
        self.logger = logger
        self.agentClient = AgentClient(configuration: configuration, logger: logger)
    }

    /// Check the current VM status
    public func status() async throws -> VMStatus {
        let result = try await runSetupScript(command: "status")

        // Parse the output to determine status
        let output = result.stdout.lowercased()
        if output.contains("running") {
            return .running
        } else if output.contains("stopped") {
            return .stopped
        } else if output.contains("not found") || output.contains("does not exist") {
            return .notFound
        }

        // If we can't determine from output, check if VM exists using limactl
        let limaResult = try await runLimactl(arguments: ["list", "--json"])
        if limaResult.stdout.contains(configuration.vmName) {
            // VM exists, check if running
            if limaResult.stdout.contains("\"status\":\"Running\"") {
                return .running
            } else {
                return .stopped
            }
        }

        return .notFound
    }

    /// Create the VM if it doesn't exist
    public func create() async throws {
        logger.info("Creating VM", metadata: ["vmName": "\(configuration.vmName)"])
        _ = try await runSetupScript(command: "create")
        logger.info("VM created successfully")
    }

    /// Start the VM
    public func start() async throws {
        logger.info("Starting VM", metadata: ["vmName": "\(configuration.vmName)"])
        _ = try await runSetupScript(command: "start")
        logger.info("VM started successfully")
    }

    /// Stop the VM
    public func stop() async throws {
        logger.info("Stopping VM", metadata: ["vmName": "\(configuration.vmName)"])
        _ = try await runSetupScript(command: "stop")
        logger.info("VM stopped successfully")
    }

    /// Delete the VM
    public func delete() async throws {
        logger.info("Deleting VM", metadata: ["vmName": "\(configuration.vmName)"])
        _ = try await runSetupScript(command: "delete")
        logger.info("VM deleted successfully")
    }

    /// Ensures the VM is running and the agent is ready.
    /// This is idempotent - can be called multiple times safely.
    public func ensureRunning() async throws {
        // Fast path: when using existing VM, skip shell script checks
        // and just verify agent is reachable
        if configuration.useExistingVM {
            try await waitForAgent()
            return
        }

        try configuration.validate()

        let currentStatus = try await status()

        switch currentStatus {
        case .running:
            logger.info("VM is already running")
        case .stopped:
            logger.info("VM is stopped, starting...")
            try await start()
        case .notFound:
            logger.info("VM not found, creating and starting...")
            try await create()
            try await start()
        }

        // Wait for the agent to be ready
        try await waitForAgent()
    }

    /// Wait for the agent to become ready by polling the gRPC endpoint
    public func waitForAgent() async throws {
        logger.info("Waiting for agent to be ready", metadata: [
            "host": "\(configuration.agentHost)",
            "port": "\(configuration.agentPort)",
            "timeout": "\(configuration.agentTimeout)s"
        ])

        let startTime = Date()
        let timeoutDate = startTime.addingTimeInterval(TimeInterval(configuration.agentTimeout))
        var lastError: Error?

        while Date() < timeoutDate {
            do {
                _ = try await agentClient.getAgentVersion()
                logger.info("Agent is ready")
                return
            } catch {
                lastError = error
                logger.debug("Agent not ready yet, retrying...", metadata: ["error": "\(error)"])
                try await Task.sleep(for: .seconds(2))
            }
        }

        throw VMError.agentTimeout(
            timeout: configuration.agentTimeout,
            lastError: lastError
        )
    }

    /// Run a setup-dev-vm.sh command
    private func runSetupScript(command: String) async throws -> ScriptResult {
        let scriptPath = configuration.setupScriptPath
        let vmPath = configuration.vmPath

        logger.debug("Running setup script", metadata: [
            "command": "\(command)",
            "script": "\(scriptPath)"
        ])

        // Use bash -c to change directory and run the script with VM_NAME env var
        let bashCommand = "cd \"\(vmPath)\" && VM_NAME=\"\(configuration.vmName)\" \"\(scriptPath)\" \(command)"

        let result = try await Subprocess.run(
            .name("bash"),
            arguments: Subprocess.Arguments(["-c", bashCommand]),
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        let stdout = result.standardOutput ?? ""
        let stderr = result.standardError ?? ""

        if !result.terminationStatus.isSuccess {
            let exitCode: Int32 = switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                code
            }
            logger.error("Setup script failed", metadata: [
                "command": "\(command)",
                "exitCode": "\(exitCode)",
                "stderr": "\(stderr)"
            ])
            throw VMError.scriptFailed(
                command: command,
                exitCode: exitCode,
                stderr: stderr
            )
        }

        return ScriptResult(stdout: stdout, stderr: stderr)
    }

    /// Run a limactl command directly
    private func runLimactl(arguments: [String]) async throws -> ScriptResult {
        let result = try await Subprocess.run(
            .name("limactl"),
            arguments: Subprocess.Arguments(arguments),
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        let stdout = result.standardOutput ?? ""
        let stderr = result.standardError ?? ""

        return ScriptResult(stdout: stdout, stderr: stderr)
    }
}

/// Result of running a script
private struct ScriptResult {
    let stdout: String
    let stderr: String
}

/// VM lifecycle errors
public enum VMError: Error, CustomStringConvertible {
    case scriptFailed(command: String, exitCode: Int32, stderr: String)
    case vmNotFound(name: String)
    case agentTimeout(timeout: Int, lastError: Error?)

    public var description: String {
        switch self {
        case .scriptFailed(let command, let exitCode, let stderr):
            return "Setup script command '\(command)' failed with exit code \(exitCode): \(stderr)"
        case .vmNotFound(let name):
            return "VM '\(name)' not found and E2E_USE_EXISTING_VM is set"
        case .agentTimeout(let timeout, let lastError):
            var msg = "Agent did not become ready within \(timeout) seconds"
            if let error = lastError {
                msg += ". Last error: \(error)"
            }
            return msg
        }
    }
}
