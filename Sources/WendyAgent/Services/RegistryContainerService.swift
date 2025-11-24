import Foundation
import Logging
import ServiceLifecycle
import Subprocess

/// Service that manages the dev registry via systemd
struct RegistryContainerService: Service {
    let logger = Logger(label: "registry-container")
    let serviceName = "wendyos-dev-registry"
    let healthCheckInterval: Duration = .seconds(30)

    func run() async {
        logger.info("Starting registry container service")

        // Ensure registry is running
        do {
            try await startRegistry()
            logger.info("✅ Registry service is running")
        } catch {
            logger.error("Failed to start registry service: \(error)")
            logger.warning("Registry will not be available, but agent will continue")
            // Don't throw - registry is optional in dev mode
        }

        // Monitor health and keep service alive
        while !Task.isCancelled {
            try await Task.sleep(for: healthCheckInterval)

            do {
                let isActive = try await checkRegistryActive()
                if !isActive {
                    logger.warning("Registry service stopped, attempting restart...")
                    do {
                        try await startRegistry()
                        logger.info("Registry service restarted")
                    } catch {
                        logger.error("Failed to restart registry: \(error)")
                        // Continue monitoring, will try again next cycle
                    }
                }
            } catch {
                logger.error("Health check failed: \(error)")
                // Continue monitoring
            }
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
            .name("systemctl"),
            arguments: ["start", serviceName],
            output: .discarded
        )

        guard result.terminationStatus.isSuccess else {
            throw RegistryError.commandFailed("systemctl start failed")
        }
    }

    /// Stop the registry systemd service
    private func stopRegistry() async throws {
        let result = try await Subprocess.run(
            .name("systemctl"),
            arguments: ["stop", serviceName],
            output: .discarded
        )

        guard result.terminationStatus.isSuccess else {
            throw RegistryError.commandFailed("systemctl stop failed")
        }
    }

    /// Check if the registry service is active
    private func checkRegistryActive() async throws -> Bool {
        let result = try await Subprocess.run(
            .name("systemctl"),
            arguments: ["is-active", serviceName],
            output: .discarded
        )

        // is-active returns 0 if active, non-zero otherwise
        return result.terminationStatus.isSuccess
    }

    enum RegistryError: Error, CustomStringConvertible {
        case commandFailed(String)

        var description: String {
            switch self {
            case .commandFailed(let message):
                return "Command failed: \(message)"
            }
        }
    }
}
