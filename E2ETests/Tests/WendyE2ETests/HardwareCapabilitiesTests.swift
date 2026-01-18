import E2ETestHarness
import Testing
import WendyAgentGRPC

/// Tests for hardware capabilities queries
@Suite("Hardware Capabilities Tests", .tags(.e2e, .hardware), .serialized)
struct HardwareCapabilitiesTests {
    let configuration: TestConfiguration
    let vmManager: VMLifecycleManager
    let agentClient: AgentClient

    init() async throws {
        configuration = TestConfiguration.fromEnvironment()
        vmManager = VMLifecycleManager(configuration: configuration)
        agentClient = AgentClient(configuration: configuration)

        // Ensure VM is running before tests
        try await vmManager.ensureRunning()
    }

    @Test("Query hardware capabilities")
    func queryHardwareCapabilities() async throws {
        let capabilities = try await agentClient.getHardware()

        print("Hardware Capabilities:")
        for capability in capabilities {
            print("  Category: \(capability.category)")
            print("    Device Path: \(capability.devicePath)")
            print("    Description: \(capability.description_p)")
            for (key, value) in capability.properties {
                print("    \(key): \(value)")
            }
        }
    }

    @Test("Hardware capabilities API responds")
    func hardwareAPIResponds() async throws {
        // The API should always respond, even if no capabilities are found
        let capabilities = try await agentClient.getHardware()

        // VM may or may not have hardware capabilities, the test passes if the query succeeds
        print("Found \(capabilities.count) hardware capabilities")
    }

    @Test("Hardware info is consistent across calls")
    func hardwareInfoIsConsistent() async throws {
        let capabilities1 = try await agentClient.getHardware()
        let capabilities2 = try await agentClient.getHardware()

        // Hardware capabilities should be consistent
        #expect(capabilities1.count == capabilities2.count,
                "Hardware capability count should be consistent")
    }

    @Test("Hardware capabilities have valid categories")
    func hardwareCapabilitiesHaveValidCategories() async throws {
        let capabilities = try await agentClient.getHardware()

        // Known hardware categories
        let knownCategories = ["gpu", "usb", "i2c", "gpio", "camera", "cpu", "memory", "disk", "network"]

        for capability in capabilities {
            // Category should either be known or at least non-empty
            let hasKnownCategory = knownCategories.contains { cat in
                capability.category.lowercased().contains(cat)
            }
            #expect(hasKnownCategory || !capability.category.isEmpty,
                    "Hardware capability should have a known or non-empty category")
        }
    }

    @Test("Hardware capabilities have device paths when applicable")
    func hardwareCapabilitiesHaveDevicePaths() async throws {
        let capabilities = try await agentClient.getHardware()

        for capability in capabilities {
            // Log device paths for debugging
            if !capability.devicePath.isEmpty {
                print("Found device at path: \(capability.devicePath)")
            }
        }
    }
}

// MARK: - Test Tags

extension Tag {
    @Tag static var hardware: Self
}
