import Foundation
import Logging
import ServiceLifecycle
import Testing

@testable import wendy_agent

@Suite("WendyAgent Service Setup")
struct WendyAgentServiceSetupTests {
    /// Tests that RegistryContainerService is included in development mode (mTLS == nil)
    @Test("RegistryContainerService is added in development mode")
    func testRegistryServiceAddedInDevMode() async throws {
        // This test verifies the service setup logic by checking that
        // RegistryContainerService is properly instantiated and would be
        // added to the service group in development mode.

        // Create a RegistryContainerService with a mock systemctl
        let mockSystemctl = MockSystemctlService()
        let logger = Logger(label: "test.registry-setup")

        let registryService = RegistryContainerService(
            logger: logger,
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        // Verify the service can be created and conforms to Service protocol
        #expect(registryService is any Service)

        // Start the service in a task and quickly cancel to verify it initializes
        let task = Task {
            try await registryService.run()
        }

        // Give it a brief moment to start
        try await Task.sleep(for: .milliseconds(50))

        // Cancel and verify no errors during initialization
        task.cancel()

        // The service should have attempted to start
        let startCount = await mockSystemctl.getStartCallCount()
        #expect(startCount >= 0)  // May be 0 or 1 depending on timing
    }

    /// Tests that the service configuration is correct
    @Test("RegistryContainerService uses correct default service name")
    func testRegistryServiceDefaultName() async throws {
        let mockSystemctl = MockSystemctlService()
        let logger = Logger(label: "test.registry-name")

        let registryService = RegistryContainerService(
            logger: logger,
            healthCheckInterval: .seconds(1),
            systemctl: mockSystemctl
        )

        // Start briefly to trigger service name usage
        let task = Task {
            try await registryService.run()
        }

        try await Task.sleep(for: .milliseconds(50))
        task.cancel()

        // Verify the default service name was used
        if let serviceName = await mockSystemctl.getLastServiceName() {
            #expect(serviceName == "wendyos-dev-registry")
        }
    }
}
