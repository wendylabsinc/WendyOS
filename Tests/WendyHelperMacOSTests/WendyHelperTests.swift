#if os(macOS)
    import WendyShared
    import Foundation
    import Logging
    import Testing

    @testable import wendy_helper

    @Suite("Wendy Helper Integration Tests")
    struct WendyHelperIntegrationTests {

        private func createTestLogger() -> Logger {
            return Logger(label: "testwendy.helper")
        }

        @Test("WendyHelperDaemon starts and initializes all services")
        func testDaemonStartsServices() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            let usbMonitor = MockUSBMonitorService()
            let ipManager = PlatformIPAddressManager(logger: logger)
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            let daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: mockDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger,
                networkInterfaceMonitor: nil
            )

            try await daemon.start()

            // Verify daemon started the USB monitor service
            #expect(await usbMonitor.startCallCount >= 1)

            await daemon.stop()
        }

        @Test("WendyHelperDaemon handles device discovery failures gracefully")
        func testDaemonHandlesDiscoveryFailures() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            await mockDiscovery.setShouldFailUSBDiscovery(true)

            let usbMonitor = MockUSBMonitorService()
            let ipManager = PlatformIPAddressManager(logger: logger)
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            let daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: mockDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger,
                networkInterfaceMonitor: nil
            )

            // Should start successfully even with failing discovery
            try await daemon.start()

            // Should have started the USB monitor successfully
            #expect(await usbMonitor.startCallCount >= 1)

            await daemon.stop()
        }

        @Test("WendyHelperDaemon responds to Wendy device connections and disconnections")
        func testDaemonRespondsToDeviceConnections() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            let usbMonitor = MockUSBMonitorService()
            let ipManager = MockIPAddressManager()  // Use mock here!
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            #if os(macOS)
                let mockNetworkInterfaceMonitor = MockNetworkInterfaceMonitor()
                let daemon = WendyHelperDaemon(
                    usbMonitor: usbMonitor,
                    deviceDiscovery: mockDiscovery,
                    networkDaemonClient: networkDaemonClient,
                    ipManager: ipManager,
                    logger: logger,
                    networkInterfaceMonitor: mockNetworkInterfaceMonitor
                )
            #else
                let daemon = WendyHelperDaemon(
                    usbMonitor: usbMonitor,
                    deviceDiscovery: mockDiscovery,
                    networkDaemonClient: networkDaemonClient,
                    ipManager: ipManager,
                    logger: logger
                )
            #endif

            try await daemon.start()

            // Let initial scan complete
            try await Task.sleep(for: .milliseconds(60))

            // Create the device manually first
            let testDevice = USBDevice(name: "Wendy Device", vendorId: 0x1D6B, productId: 0x0104)
            let deviceInfo = USBDeviceInfo(from: testDevice)
            _ = deviceInfo.id

            // 1. Add corresponding ethernet interface for this device!
            let mockInterface = EthernetInterface(
                name: "en5",
                displayName: "Wendy Ethernet",  // Must contain "Wendy" for isWendyDevice
                interfaceType: "Ethernet",
                macAddress: "02:00:00:00:00:01"
            )
            await mockDiscovery.addMockEthernetInterface(mockInterface)

            // 2. Simulate Wendy USB device connection FIRST (so it goes into pendingUSBDevices)
            await mockDiscovery.addMockUSBDevice(testDevice)
            await usbMonitor.simulateDeviceConnection(deviceInfo)

            // Small delay to ensure USB device is processed
            try await Task.sleep(for: .milliseconds(50))

            #if os(macOS)
                // 3. ✅ Simulate network interface appearance (triggers configureWendyDevice)
                // Ensure the mock monitor is running before simulating the event
                let isRunning = await mockNetworkInterfaceMonitor.getIsRunning()
                #expect(isRunning, "Network interface monitor should be running")
                await mockNetworkInterfaceMonitor.simulateInterfaceAppearance("en5")
            #endif

            // Wait for connection processing
            try await Task.sleep(for: .milliseconds(200))

            // Verify IP assignment happened (network config via XPC not testable here)
            #expect(await ipManager.assignCallCount >= 1)

            // 2. ✅ Test DISCONNECTION flow
            // Simulate device disconnection event
            await usbMonitor.simulateDeviceDisconnection(deviceInfo)

            // Wait for disconnection processing
            try await Task.sleep(for: .milliseconds(200))

            await daemon.stop()

            // ✅ Verify IP cleanup happened (network cleanup via XPC not testable here)
            #expect(await ipManager.releaseCallCount >= 1)

            let releaseHistory = await ipManager.getReleaseHistory()
            #expect(releaseHistory.contains { $0.bsdName == "en5" })
        }

        @Test("WendyHelperDaemon stops cleanly and cancels monitoring")
        func testDaemonStopsCleanly() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            let usbMonitor = MockUSBMonitorService()
            let ipManager = PlatformIPAddressManager(logger: logger)
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            #if os(macOS)
                let mockNetworkInterfaceMonitor = MockNetworkInterfaceMonitor()
                let daemon = WendyHelperDaemon(
                    usbMonitor: usbMonitor,
                    deviceDiscovery: mockDiscovery,
                    networkDaemonClient: networkDaemonClient,
                    ipManager: ipManager,
                    logger: logger,
                    networkInterfaceMonitor: mockNetworkInterfaceMonitor
                )
            #else
                let daemon = WendyHelperDaemon(
                    usbMonitor: usbMonitor,
                    deviceDiscovery: mockDiscovery,
                    networkDaemonClient: networkDaemonClient,
                    ipManager: ipManager,
                    logger: logger
                )
            #endif

            try await daemon.start()

            // Let it run briefly
            try await Task.sleep(for: .milliseconds(100))

            // Verify USB monitor is running
            #expect(await usbMonitor.getIsRunning() == true)

            await daemon.stop()

            // Wait a bit more
            try await Task.sleep(for: .milliseconds(100))

            // USB monitor should have been stopped
            #expect(await usbMonitor.getIsRunning() == false)
        }

        @Test("Multiple start calls are handled safely")
        func testMultipleStartCallsSafety() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            let usbMonitor = MockUSBMonitorService()
            let ipManager = PlatformIPAddressManager(logger: logger)
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            let daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: mockDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger,
                networkInterfaceMonitor: nil
            )

            // Start multiple times
            try await daemon.start()
            try await daemon.start()

            try await Task.sleep(for: .milliseconds(100))

            await daemon.stop()

            // Should still work correctly - USB monitor should have started
            #expect(await usbMonitor.startCallCount >= 1)
        }

        @Test("Daemon stops quickly, not after full polling interval")
        func testDaemonStopsQuickly() async throws {
            let logger = createTestLogger()
            let mockDiscovery = MockUSBDeviceDiscovery(logger: logger)
            let usbMonitor = PlatformUSBMonitor(
                deviceDiscovery: mockDiscovery,
                logger: logger,
                pollingInterval: .seconds(50)
            )
            let ipManager = PlatformIPAddressManager(logger: logger)
            let networkDaemonClient = MockNetworkDaemonClient(logger: logger)

            let daemon = WendyHelperDaemon(
                usbMonitor: usbMonitor,
                deviceDiscovery: mockDiscovery,
                networkDaemonClient: networkDaemonClient,
                ipManager: ipManager,
                logger: logger,
                networkInterfaceMonitor: nil
            )

            let start = ContinuousClock.now

            try await daemon.start()
            try await Task.sleep(for: .milliseconds(100))

            await daemon.stop()  // Should be fast

            let elapsed = ContinuousClock.now - start
            #expect(elapsed < .seconds(1))  // Should stop in < 1 second, not 30!
        }
    }
#endif
