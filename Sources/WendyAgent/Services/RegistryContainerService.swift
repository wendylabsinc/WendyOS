import Foundation
import Logging
import ServiceLifecycle
import Subprocess

/// Service that manages the dev registry via systemd
struct RegistryContainerService: Service {
    let logger = Logger(label: "registry-container")
    let serviceName = "wendyos-dev-registry"
    let healthCheckInterval: Duration = .seconds(30)

    func run() async throws {
        logger.info("Starting registry container service")

        // Ensure registry is running
        do {
            try await startRegistry()

            // Verify it actually started
            try await Task.sleep(for: .seconds(2))
            let state = try await getServiceState()
            if state == "active" {
                logger.info("✅ Registry service is running")
            } else {
                logger.warning("Registry service started but in '\(state)' state")
            }
        } catch {
            logger.error("Failed to start registry service: \(error)")
            logger.warning("Registry will not be available, but agent will continue")
            // Don't throw - registry is optional in dev mode
        }

        // Monitor health and keep service alive
        try await withGracefulShutdownHandler {
            while !Task.isCancelled {
                try await Task.sleep(for: healthCheckInterval)

                do {
                    let state = try await getServiceState()

                    // Only restart if truly stopped or failed (not transitional states)
                    if state == "inactive" || state == "failed" {
                        logger.warning(
                            "Registry service in '\(state)' state, attempting restart..."
                        )
                        do {
                            try await startRegistry()
                            try await Task.sleep(for: .seconds(2))
                            let newState = try await getServiceState()
                            if newState == "active" {
                                logger.info("Registry service restarted successfully")
                            } else {
                                logger.warning(
                                    "Registry service restarted but in '\(newState)' state"
                                )
                            }
                        } catch {
                            logger.error("Failed to restart registry: \(error)")
                            // Continue monitoring, will try again next cycle
                        }
                    } else if state != "active" {
                        logger.debug(
                            "Registry service in transitional state",
                            metadata: ["state": "\(state)"]
                        )
                    }
                } catch {
                    logger.error("Health check failed: \(error)")
                    // Continue monitoring
                }
            }
        } onGracefulShutdown: {
            self.logger.info("Registry container service shutting down gracefully")
        }

        logger.info("Registry container service shutting down")

        // Stop registry on shutdown
        do {
            try await stopRegistry()
            logger.info("Registry service stopped")
        } catch {
            logger.warning("Failed to stop registry service: \(error)")
        }
    }

    /// Start the registry systemd service
    private func startRegistry() async throws {
        let result = try await Subprocess.run(
            .path("/usr/bin/systemctl"),
            arguments: ["start", serviceName],
            output: .string(limit: 1000),
            error: .string(limit: 1000)
        )

        guard result.terminationStatus.isSuccess else {
            let stderr = result.standardError ?? ""
            let stdout = result.standardOutput ?? ""
            throw RegistryError.commandFailed(
                "systemctl start failed",
                stdout: stdout,
                stderr: stderr
            )
        }
    }

    /// Stop the registry systemd service
    private func stopRegistry() async throws {
        let result = try await Subprocess.run(
            .path("/usr/bin/systemctl"),
            arguments: ["stop", serviceName],
            output: .string(limit: 1000),
            error: .string(limit: 1000)
        )

        guard result.terminationStatus.isSuccess else {
            let stderr = result.standardError ?? ""
            let stdout = result.standardOutput ?? ""
            throw RegistryError.commandFailed(
                "systemctl stop failed",
                stdout: stdout,
                stderr: stderr
            )
        }
    }

    /// Get the current state of the registry service
    private func getServiceState() async throws -> String {
        let result = try await Subprocess.run(
            .path("/usr/bin/systemctl"),
            arguments: ["show", serviceName, "--property=ActiveState"],
            output: .string(limit: 100),
            error: .string(limit: 1000)
        )

        guard result.terminationStatus.isSuccess else {
            let stderr = result.standardError ?? ""
            throw RegistryError.commandFailed(
                "systemctl show failed",
                stdout: "",
                stderr: stderr
            )
        }

        // Parse "ActiveState=active" -> "active"
        let output = result.standardOutput ?? ""
        let state = output
            .trimmingCharacters(in: CharacterSet.whitespacesAndNewlines)
            .replacingOccurrences(of: "ActiveState=", with: "")

        return state.isEmpty ? "unknown" : state
    }

    enum RegistryError: Error, CustomStringConvertible {
        case commandFailed(String, stdout: String, stderr: String)

        var description: String {
            switch self {
            case .commandFailed(let message, let stdout, let stderr):
                var desc = "Command failed: \(message)"
                if !stdout.isEmpty {
                    desc += "\nstdout: \(stdout)"
                }
                if !stderr.isEmpty {
                    desc += "\nstderr: \(stderr)"
                }
                return desc
            }
        }
    }
}
