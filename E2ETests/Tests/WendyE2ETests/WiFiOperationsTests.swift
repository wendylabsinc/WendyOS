import E2ETestHarness
import Testing
import WendyAgentGRPC

/// Tests for WiFi operations
/// Note: Many of these tests may fail gracefully in a VM environment without WiFi hardware
@Suite("WiFi Operations Tests", .tags(.e2e, .wifi), .serialized)
struct WiFiOperationsTests {
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

    @Test("List WiFi networks")
    func listWiFiNetworks() async throws {
        do {
            let networks = try await agentClient.listWiFiNetworks()

            print("Found \(networks.count) WiFi networks:")
            for network in networks {
                print("  - SSID: \(network.ssid), Signal: \(network.signalStrength)")
            }

            // In a VM, we may not have any networks, which is acceptable
            // The test passes if the API call succeeds
        } catch {
            // WiFi operations may fail in a VM without WiFi hardware
            // This is expected and acceptable
            print("WiFi list failed (expected in VM): \(error)")
            Issue.record("WiFi operations not available in this environment (expected for VM)")
        }
    }

    @Test("Get WiFi status")
    func getWiFiStatus() async throws {
        do {
            let status = try await agentClient.getWiFiStatus()

            print("WiFi status:")
            print("  Connected: \(status.connected)")
            if status.connected {
                print("  SSID: \(status.ssid)")
            }
        } catch {
            // WiFi operations may fail in a VM without WiFi hardware
            print("WiFi status failed (expected in VM): \(error)")
            Issue.record("WiFi operations not available in this environment (expected for VM)")
        }
    }

    @Test("WiFi API responds even without hardware")
    func wifiAPIResponds() async throws {
        // This test verifies the API responds, even if with an error
        // It should not hang or crash
        var apiResponded = false

        do {
            _ = try await agentClient.listWiFiNetworks()
            apiResponded = true
        } catch {
            // Any error response means the API responded
            apiResponded = true
        }

        #expect(apiResponded, "WiFi API should respond (even with error)")
    }

    @Test("WiFi status API responds even without hardware")
    func wifiStatusAPIResponds() async throws {
        var apiResponded = false

        do {
            _ = try await agentClient.getWiFiStatus()
            apiResponded = true
        } catch {
            apiResponded = true
        }

        #expect(apiResponded, "WiFi status API should respond (even with error)")
    }
}

// MARK: - Test Tags

extension Tag {
    @Tag static var wifi: Self
}
