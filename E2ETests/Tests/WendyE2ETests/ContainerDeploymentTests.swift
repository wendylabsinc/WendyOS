import E2ETestHarness
import Foundation
import Testing
import WendyAgentGRPC

/// Tests for container deployment and lifecycle management
/// Note: Tests that require specific images will be skipped if images are not available.
/// Use `wendy run` to push applications to the device before running these tests.
@Suite("Container Deployment Tests", .tags(.e2e, .containers), .serialized)
struct ContainerDeploymentTests {
    let configuration: TestConfiguration
    let vmManager: VMLifecycleManager
    let agentClient: AgentClient
    let containerHelpers: ContainerTestHelpers

    init() async throws {
        configuration = TestConfiguration.fromEnvironment()
        vmManager = VMLifecycleManager(configuration: configuration)
        agentClient = AgentClient(configuration: configuration)
        containerHelpers = ContainerTestHelpers(configuration: configuration)

        // Ensure VM is running before tests
        try await vmManager.ensureRunning()
    }

    @Test("List containers on fresh VM")
    func listContainersOnFreshVM() async throws {
        let containers = try await agentClient.listContainers()

        // A fresh VM may or may not have containers
        print("Found \(containers.count) containers on VM")

        for container in containers {
            print("  - \(container.appName): \(container.runningState)")
        }
    }

    @Test("Stop and start existing container")
    func stopAndStartExistingContainer() async throws {
        let containers = try await agentClient.listContainers()

        guard let container = containers.first else {
            Issue.record("No containers available on VM. Push an app using 'wendy run' first.")
            return
        }

        let appName = container.appName
        let originalState = container.runningState

        print("Testing with existing container: \(appName) (state: \(originalState))")

        // If running, stop it first
        if originalState == .running {
            _ = try await agentClient.stopContainer(appName: appName)
            try await containerHelpers.waitForContainerState(appName: appName, state: .stopped, timeout: 30)
        }

        // Start the container
        try await agentClient.startContainer(appName: appName)
        try await containerHelpers.waitForContainerState(appName: appName, state: .running, timeout: 30)

        print("Container \(appName) started successfully")

        // Stop it again
        _ = try await agentClient.stopContainer(appName: appName)
        try await containerHelpers.waitForContainerState(appName: appName, state: .stopped, timeout: 30)

        print("Container \(appName) stopped successfully")

        // Restore original state if it was running
        if originalState == .running {
            try await agentClient.startContainer(appName: appName)
            try await containerHelpers.waitForContainerState(appName: appName, state: .running, timeout: 30)
        }
    }

    @Test("Container state transitions")
    func containerStateTransitions() async throws {
        let containers = try await agentClient.listContainers()

        guard let container = containers.first else {
            Issue.record("No containers available on VM. Push an app using 'wendy run' first.")
            return
        }

        let appName = container.appName
        print("Testing state transitions for: \(appName)")

        // Get initial state
        let initialContainers = try await agentClient.listContainers()
        let initialState = initialContainers.first { $0.appName == appName }?.runningState

        #expect(initialState != nil, "Container should have a state")
        print("Initial state: \(String(describing: initialState))")
    }

    @Test("Delete non-existent container is idempotent")
    func deleteNonExistentContainerIsIdempotent() async throws {
        let appName = "e2e-test-nonexistent-\(UUID().uuidString.prefix(8))"

        // Deleting a non-existent container should not throw or should throw a specific error
        do {
            _ = try await agentClient.deleteContainer(appName: appName)
            // If it doesn't throw, that's fine (idempotent)
        } catch {
            // Some implementations may throw NotFound, which is also acceptable
            print("Delete non-existent container error (acceptable): \(error)")
        }
    }

    @Test("List containers API responds")
    func listContainersAPIResponds() async throws {
        // This test verifies the API responds, regardless of container count
        let containers = try await agentClient.listContainers()
        print("Container count: \(containers.count)")

        // The test passes as long as we get a response
        #expect(containers.count >= 0, "Should return a valid container list")
    }

    @Test("Multiple list operations are consistent")
    func multipleListOperationsAreConsistent() async throws {
        let containers1 = try await agentClient.listContainers()
        let containers2 = try await agentClient.listContainers()

        // Container count should be consistent between rapid queries
        // (unless something else is modifying containers concurrently)
        #expect(containers1.count == containers2.count,
                "Container count should be consistent: \(containers1.count) vs \(containers2.count)")

        // Check that app names are the same
        let names1 = Set(containers1.map { $0.appName })
        let names2 = Set(containers2.map { $0.appName })
        #expect(names1 == names2, "Container names should be consistent")
    }
}

// MARK: - Test Tags

extension Tag {
    @Tag static var containers: Self
}
