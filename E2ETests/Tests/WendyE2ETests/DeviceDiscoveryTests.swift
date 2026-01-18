import E2ETestHarness
import Testing
import WendyAgentGRPC

/// Tests for mDNS device discovery functionality
@Suite("Device Discovery Tests", .tags(.e2e), .serialized)
struct DeviceDiscoveryTests {
    let configuration: TestConfiguration
    let vmManager: VMLifecycleManager
    let discoveryHelper: DeviceDiscoveryHelper

    init() async throws {
        configuration = TestConfiguration.fromEnvironment()
        vmManager = VMLifecycleManager(configuration: configuration)
        discoveryHelper = DeviceDiscoveryHelper()

        // Ensure VM is running before tests
        try await vmManager.ensureRunning()
    }

    @Test("Discover VM via mDNS")
    func discoverVMViaMDNS() async throws {
        let devices = try await discoveryHelper.discoverDevices()

        // We should find at least one device (our test VM)
        #expect(!devices.isEmpty, "Expected to discover at least one WendyOS device")

        // Log discovered devices for debugging
        for device in devices {
            print("Discovered device: \(device.hostname):\(device.port)")
            print("  UUID: \(device.uuid ?? "unknown")")
            print("  Name: \(device.name ?? "unknown")")
            print("  Version: \(device.version ?? "unknown")")
            print("  Platform: \(device.platform ?? "unknown")")
        }
    }

    @Test("Verify TXT records contain required fields")
    func verifyTXTRecords() async throws {
        let devices = try await discoveryHelper.discoverDevices()

        guard let device = devices.first else {
            Issue.record("No devices discovered to verify TXT records")
            return
        }

        // Verify TXT record fields exist (they may be empty in some configurations)
        #expect(device.txtRecords.keys.contains("uuid") || device.uuid != nil,
                "Expected UUID in TXT records or device ID")

        // The port should be the gRPC port
        #expect(device.port == configuration.agentPort || device.port == 50051,
                "Expected advertised port to be the agent gRPC port")
    }

    @Test("Verify advertised port matches configuration")
    func verifyAdvertisedPort() async throws {
        let devices = try await discoveryHelper.discoverDevices()

        guard let device = devices.first else {
            Issue.record("No devices discovered to verify port")
            return
        }

        // The advertised port should match our expected agent port
        #expect(device.port == configuration.agentPort,
                "Advertised port (\(device.port)) should match configured port (\(configuration.agentPort))")
    }

    @Test("Wait for device by hostname")
    func waitForDeviceByHostname() async throws {
        // Try to wait for a device containing "wendyos" or "lima" in hostname
        // This may timeout if the VM doesn't advertise via mDNS with these hostnames
        do {
            let device = try await discoveryHelper.waitForDevice(
                withHostname: "wendyos",
                timeout: 10
            )
            #expect(!device.hostname.isEmpty)
        } catch is DiscoveryError {
            // Try with lima hostname as fallback
            do {
                let device = try await discoveryHelper.waitForDevice(
                    withHostname: "lima",
                    timeout: 10
                )
                #expect(!device.hostname.isEmpty)
            } catch {
                // Not finding via mDNS is acceptable - the VM may not advertise
                Issue.record("Could not find device via mDNS hostname search (this may be expected)")
            }
        }
    }
}

// MARK: - Test Tags

extension Tag {
    @Tag static var e2e: Self
    @Tag static var discovery: Self
}
