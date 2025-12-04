import Foundation
import Logging
import Testing

@testable import wendy_agent

@Suite("Registry Container Service")
struct RegistryContainerServiceTests {
    private func createTestLogger() -> Logger {
        return Logger(label: "test.registry-container")
    }

    @Test("Service starts successfully with active state")
    func testSuccessfulStart() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .seconds(1),  // Short interval for testing
            systemctl: mockSystemctl
        )

        // Run service in background task and cancel quickly
        let task = Task {
            try await service.run()
        }

        // Give it more time for start verification to complete (needs 2 seconds sleep + state check)
        try await Task.sleep(for: .seconds(3))

        // Cancel the service
        task.cancel()

        // Verify start was called and state was checked
        let startCount = await mockSystemctl.getStartCallCount()
        let getStateCount = await mockSystemctl.getStateCallCount()
        #expect(startCount >= 1)
        #expect(getStateCount >= 1)
    }

    @Test("Service handles start failure gracefully")
    func testStartFailureIsNonFatal() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        await mockSystemctl.setShouldFailStart(true)

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        // Service should not throw even when start fails
        let task = Task {
            try await service.run()
        }

        // Give it time to attempt start
        try await Task.sleep(for: .milliseconds(100))

        // Cancel the service
        task.cancel()

        // Verify start was attempted
        let startCount = await mockSystemctl.getStartCallCount()
        #expect(startCount >= 1)
    }

    @Test("Service uses correct service name")
    func testServiceNameIsPassedCorrectly() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        let customServiceName = "test-registry-service"

        let service = RegistryContainerService(
            logger: createTestLogger(),
            serviceName: customServiceName,
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        // Give it time to start
        try await Task.sleep(for: .milliseconds(100))

        task.cancel()

        // Verify the correct service name was used
        let lastService = await mockSystemctl.getLastServiceName()
        #expect(lastService == customServiceName)
    }

    @Test("Service stops on shutdown")
    func testStopOnShutdown() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        // Give it time to start
        try await Task.sleep(for: .milliseconds(100))

        // Cancel and wait for shutdown
        task.cancel()
        _ = try? await task.value

        // Verify stop was called during shutdown
        let stopCount = await mockSystemctl.getStopCallCount()
        #expect(stopCount >= 1)
    }

    @Test("Service handles stop failure gracefully")
    func testStopFailureIsNonFatal() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        await mockSystemctl.setShouldFailStop(true)

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        try await Task.sleep(for: .milliseconds(100))

        // Cancel and wait - should not throw despite stop failure
        task.cancel()
        _ = try? await task.value

        // Verify stop was attempted
        let stopCount = await mockSystemctl.getStopCallCount()
        #expect(stopCount >= 1)
    }

    @Test("RegistryError description includes context")
    func testErrorDescription() {
        let error = RegistryContainerService.RegistryError.commandFailed(
            "test command failed",
            stdout: "output text",
            stderr: "error text"
        )

        let description = error.description

        #expect(description.contains("test command failed"))
        #expect(description.contains("output text"))
        #expect(description.contains("error text"))
    }

    @Test("RegistryError description handles empty output")
    func testErrorDescriptionWithEmptyOutput() {
        let error = RegistryContainerService.RegistryError.commandFailed(
            "test command failed",
            stdout: "",
            stderr: ""
        )

        let description = error.description

        #expect(description.contains("test command failed"))
        // Should not include stdout/stderr labels if empty
        #expect(!description.contains("stdout"))
        #expect(!description.contains("stderr"))
    }

    @Test("Service restarts when registry becomes inactive")
    func testRestartOnInactiveState() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        await mockSystemctl.setStateToReturn("active")

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .milliseconds(200),  // Fast for testing
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        // Wait for initial start to complete (needs 2 second sleep + verification)
        try await Task.sleep(for: .seconds(3))
        let initialStartCount = await mockSystemctl.getStartCallCount()
        #expect(initialStartCount >= 1)

        // Simulate registry failure
        await mockSystemctl.setStateToReturn("inactive")

        // Wait for health check to detect and restart
        // Health check runs every 200ms, restart needs 2 seconds, so wait ~3 seconds
        try await Task.sleep(for: .seconds(3))

        task.cancel()

        // Should have restarted (initial start + at least one restart)
        let finalStartCount = await mockSystemctl.getStartCallCount()
        #expect(finalStartCount > initialStartCount, "Expected restart after state became inactive")
    }

    @Test("Service does not restart on transitional states")
    func testNoRestartOnTransitionalState() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        await mockSystemctl.setStateToReturn("active")

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .milliseconds(200),
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        // Wait for initial start
        try await Task.sleep(for: .seconds(3))
        let initialStartCount = await mockSystemctl.getStartCallCount()

        // Change to transitional state (should not trigger restart)
        await mockSystemctl.setStateToReturn("activating")

        // Wait through several health check cycles
        try await Task.sleep(for: .seconds(1))

        task.cancel()

        // Should NOT have restarted (only initial start)
        let finalStartCount = await mockSystemctl.getStartCallCount()
        #expect(
            finalStartCount == initialStartCount,
            "Should not restart for transitional state 'activating'"
        )
    }

    @Test("Service handles health check errors gracefully")
    func testHealthCheckErrorHandling() async throws {
        let mockSystemctl = MockSystemctlService()
        await mockSystemctl.reset()
        await mockSystemctl.setStateToReturn("active")

        let service = RegistryContainerService(
            logger: createTestLogger(),
            healthCheckInterval: .milliseconds(200),
            systemctl: mockSystemctl
        )

        let task = Task {
            try await service.run()
        }

        // Wait for initial start
        try await Task.sleep(for: .seconds(3))

        // Make getState start failing
        await mockSystemctl.setShouldFailGetState(true)

        // Wait for health checks to encounter errors
        try await Task.sleep(for: .seconds(1))

        task.cancel()

        // Service should still be running (errors are caught and logged)
        // The important thing is that we didn't crash
        let stateCount = await mockSystemctl.getStateCallCount()
        #expect(stateCount > 1, "Health checks should have been attempted despite errors")
    }
}
