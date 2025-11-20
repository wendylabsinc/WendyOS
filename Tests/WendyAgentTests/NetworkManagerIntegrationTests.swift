import Foundation
import Testing

@testable import wendy_agent

@Suite("Network Manager Integration")
struct NetworkManagerIntegrationTests {
    @Test("Complete WiFi connection workflow with mock")
    func testCompleteWiFiWorkflow() async throws {
        // Create a mock network manager
        let mockManager = MockNetworkConnectionManager()

        // Setup mock networks
        await mockManager.addMockNetwork(ssid: "HomeNetwork", signalStrength: -45, isSecured: true)
        await mockManager.addMockNetwork(
            ssid: "GuestNetwork",
            signalStrength: -70,
            isSecured: false
        )
        await mockManager.addMockNetwork(
            ssid: "OfficeNetwork",
            signalStrength: -60,
            isSecured: true
        )

        // List networks
        let networks = try await mockManager.listWiFiNetworks()
        #expect(networks.count == 3)
        #expect(await mockManager.listWiFiNetworksCalled == true)

        // Find a specific network
        let homeNetwork = networks.first { $0.ssid == "HomeNetwork" }
        #expect(homeNetwork != nil)
        #expect(homeNetwork?.signalStrength == -45)
        #expect(homeNetwork?.isSecured == true)

        // Connect to network
        try await mockManager.connectToNetwork(ssid: "HomeNetwork", password: "password123")
        #expect(await mockManager.connectToNetworkCalled == true)
        #expect(await mockManager.lastConnectSSID == "HomeNetwork")
        #expect(await mockManager.lastConnectPassword == "password123")

        // Check connection status
        let connection = try await mockManager.getCurrentConnection()
        #expect(connection != nil)
        #expect(connection?.ssid == "HomeNetwork")
        #expect(connection?.state == .connected)
        #expect(connection?.ipAddress == "192.168.1.100")

        // Disconnect
        let disconnected = try await mockManager.disconnectFromNetwork()
        #expect(disconnected == true)
        #expect(await mockManager.disconnectFromNetworkCalled == true)

        // Verify disconnection
        let afterDisconnect = try await mockManager.getCurrentConnection()
        #expect(afterDisconnect == nil)
    }

    @Test("Error handling in network operations")
    func testNetworkErrorHandling() async throws {
        let mockManager = MockNetworkConnectionManager()

        // Set up error condition
        await mockManager.setShouldThrowError(.networkNotFound)

        // Attempt to connect should throw
        await #expect(throws: NetworkConnectionError.networkNotFound) {
            try await mockManager.connectToNetwork(ssid: "NonExistentNetwork", password: "password")
        }

        // Clear error and set authentication failure
        await mockManager.setShouldThrowError(.authenticationFailed)

        await #expect(throws: NetworkConnectionError.authenticationFailed) {
            try await mockManager.connectToNetwork(ssid: "SecureNetwork", password: "wrongpassword")
        }
    }

    @Test("SetupWiFi convenience method")
    func testSetupWiFiMethod() async throws {
        let mockManager = MockNetworkConnectionManager()

        // Setup WiFi (alias for connectToNetwork)
        try await mockManager.setupWiFi(ssid: "QuickSetupNetwork", password: "quickpass")

        #expect(await mockManager.setupWiFiCalled == true)
        #expect(await mockManager.lastSetupSSID == "QuickSetupNetwork")
        #expect(await mockManager.lastSetupPassword == "quickpass")

        // Should be connected
        let connection = try await mockManager.getCurrentConnection()
        #expect(connection?.ssid == "QuickSetupNetwork")
    }

    @Test("Network manager protocol conformance")
    func testProtocolConformance() async throws {
        // Verify that our mock conforms to the protocol
        let manager: NetworkConnectionManager = MockNetworkConnectionManager()

        // Protocol methods should be available
        _ = try await manager.listWiFiNetworks()
        try await manager.connectToNetwork(ssid: "test", password: "test")
        _ = try await manager.getCurrentConnection()
        _ = try await manager.disconnectFromNetwork()
        try await manager.setupWiFi(ssid: "test", password: "test")
    }

    @Test("Configuration integration with factory preference")
    func testConfigurationWithFactory() {
        // Create configuration with specific preference
        let config = WendyAgentConfiguration(networkManagerPreference: .preferConnMan)

        // Verify factory preference matches
        #expect(config.networkManagerPreference == .preferConnMan)

        // Test all preference mappings
        let preferences: [NetworkConnectionManagerFactory.Preference] = [
            .auto,
            .preferConnMan,
            .preferNetworkManager,
            .forceConnMan,
            .forceNetworkManager,
        ]

        for pref in preferences {
            let testConfig = WendyAgentConfiguration(networkManagerPreference: pref)
            #expect(testConfig.networkManagerPreference == pref)
        }
    }

    @Test("WiFi network discovery simulation")
    func testWiFiNetworkDiscovery() async throws {
        let mockManager = MockNetworkConnectionManager()

        // Simulate network scan with various signal strengths
        await mockManager.addMockNetwork(ssid: "StrongSignal", signalStrength: -30, isSecured: true)
        await mockManager.addMockNetwork(ssid: "MediumSignal", signalStrength: -60, isSecured: true)
        await mockManager.addMockNetwork(ssid: "WeakSignal", signalStrength: -85, isSecured: true)
        await mockManager.addMockNetwork(ssid: "OpenNetwork", signalStrength: -50, isSecured: false)

        let networks = try await mockManager.listWiFiNetworks()

        // Sort by signal strength (strongest first)
        let sortedNetworks = networks.sorted { (n1, n2) in
            guard let s1 = n1.signalStrength, let s2 = n2.signalStrength else {
                return false
            }
            return s1 > s2  // Less negative is stronger
        }

        #expect(sortedNetworks.first?.ssid == "StrongSignal")
        #expect(sortedNetworks.last?.ssid == "WeakSignal")

        // Check security
        let openNetworks = networks.filter { !$0.isSecured }
        #expect(openNetworks.count == 1)
        #expect(openNetworks.first?.ssid == "OpenNetwork")
    }

    // Helper extension for async access to mock properties
}

extension MockNetworkConnectionManager {
    func setShouldThrowError(_ error: NetworkConnectionError?) {
        self.shouldThrowError = error
    }
}
