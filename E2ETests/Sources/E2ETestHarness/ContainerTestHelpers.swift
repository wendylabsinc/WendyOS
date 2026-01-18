import Foundation
import Logging
import WendyAgentGRPC

/// Helpers for container-related E2E tests
public struct ContainerTestHelpers: Sendable {
    private let agentClient: AgentClient
    private let logger: Logger

    public init(configuration: TestConfiguration, logger: Logger = Logger(label: "E2ETestHarness.ContainerTestHelpers")) {
        self.agentClient = AgentClient(configuration: configuration, logger: logger)
        self.logger = logger
    }

    /// Wait for a container to reach a specific running state
    public func waitForContainerState(
        appName: String,
        state: AppRunningState,
        timeout: TimeInterval = 30
    ) async throws {
        let startTime = Date()
        let timeoutDate = startTime.addingTimeInterval(timeout)

        logger.info("Waiting for container state", metadata: [
            "appName": "\(appName)",
            "targetState": "\(state)",
            "timeout": "\(timeout)s"
        ])

        while Date() < timeoutDate {
            let containers = try await agentClient.listContainers()
            if let container = containers.first(where: { $0.appName == appName }) {
                if container.runningState == state {
                    logger.info("Container reached target state", metadata: [
                        "appName": "\(appName)",
                        "state": "\(state)"
                    ])
                    return
                }
                logger.debug("Container state", metadata: [
                    "appName": "\(appName)",
                    "currentState": "\(container.runningState)",
                    "targetState": "\(state)"
                ])
            } else if state == .stopped {
                // Container not found could mean it was deleted, which counts as stopped
                return
            }

            try await Task.sleep(for: .seconds(1))
        }

        throw ContainerError.stateTimeout(appName: appName, expectedState: state, timeout: timeout)
    }

    /// Clean up a container by stopping and deleting it (idempotent)
    public func cleanupContainer(appName: String) async {
        logger.info("Cleaning up container", metadata: ["appName": "\(appName)"])

        do {
            _ = try await agentClient.stopContainer(appName: appName)
        } catch {
            logger.debug("Stop container failed (may already be stopped)", metadata: [
                "appName": "\(appName)",
                "error": "\(error)"
            ])
        }

        do {
            _ = try await agentClient.deleteContainer(appName: appName)
        } catch {
            logger.debug("Delete container failed (may already be deleted)", metadata: [
                "appName": "\(appName)",
                "error": "\(error)"
            ])
        }
    }

    /// Create a test container with a simple configuration
    public func createTestContainer(
        appName: String,
        imageName: String = "busybox:latest",
        cmd: String = "sleep 3600"
    ) async throws {
        logger.info("Creating test container", metadata: [
            "appName": "\(appName)",
            "imageName": "\(imageName)"
        ])

        _ = try await agentClient.createContainer(
            appName: appName,
            imageName: imageName,
            cmd: cmd
        )
    }

    /// Run a complete container lifecycle test
    public func runContainerLifecycleTest(
        appName: String,
        imageName: String = "busybox:latest",
        cmd: String = "sleep 3600"
    ) async throws -> ContainerLifecycleResult {
        var result = ContainerLifecycleResult()

        // Cleanup any existing container with the same name
        await cleanupContainer(appName: appName)

        // Create
        do {
            try await createTestContainer(appName: appName, imageName: imageName, cmd: cmd)
            result.createSucceeded = true
        } catch {
            result.errors.append(.create(error))
            return result
        }

        // Start
        do {
            try await agentClient.startContainer(appName: appName)
            result.startSucceeded = true
            try await waitForContainerState(appName: appName, state: .running, timeout: 30)
        } catch {
            result.errors.append(.start(error))
        }

        // Stop
        do {
            _ = try await agentClient.stopContainer(appName: appName)
            result.stopSucceeded = true
            try await waitForContainerState(appName: appName, state: .stopped, timeout: 30)
        } catch {
            result.errors.append(.stop(error))
        }

        // Delete
        do {
            _ = try await agentClient.deleteContainer(appName: appName)
            result.deleteSucceeded = true
        } catch {
            result.errors.append(.delete(error))
        }

        return result
    }
}

/// Result of a container lifecycle test
public struct ContainerLifecycleResult: Sendable {
    public var createSucceeded = false
    public var startSucceeded = false
    public var stopSucceeded = false
    public var deleteSucceeded = false
    public var errors: [ContainerLifecycleError] = []

    public var allSucceeded: Bool {
        createSucceeded && startSucceeded && stopSucceeded && deleteSucceeded && errors.isEmpty
    }
}

/// Errors during container lifecycle
public enum ContainerLifecycleError: Error, Sendable {
    case create(Error)
    case start(Error)
    case stop(Error)
    case delete(Error)
}

/// Container operation errors
public enum ContainerError: Error, CustomStringConvertible {
    case stateTimeout(appName: String, expectedState: AppRunningState, timeout: TimeInterval)
    case notFound(appName: String)

    public var description: String {
        switch self {
        case .stateTimeout(let appName, let expectedState, let timeout):
            return "Container '\(appName)' did not reach state '\(expectedState)' within \(timeout) seconds"
        case .notFound(let appName):
            return "Container '\(appName)' not found"
        }
    }
}
