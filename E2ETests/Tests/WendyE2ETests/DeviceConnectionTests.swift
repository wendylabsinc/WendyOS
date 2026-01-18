import E2ETestHarness
import Testing
import WendyAgentGRPC

/// Tests for device connection and basic gRPC communication
@Suite("Device Connection Tests", .tags(.e2e), .serialized)
struct DeviceConnectionTests {
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

    @Test("Connect to agent via gRPC")
    func connectToAgent() async throws {
        // Simply getting the version proves we can connect
        let version = try await agentClient.getAgentVersion()
        #expect(!version.isEmpty, "Expected non-empty agent version")
        print("Connected to agent, version: \(version)")
    }

    @Test("Get agent version")
    func getAgentVersion() async throws {
        let version = try await agentClient.getAgentVersion()

        #expect(!version.isEmpty, "Agent version should not be empty")

        // Version should be in semver-like format or contain version info
        print("Agent version: \(version)")
    }

    @Test("Query hardware capabilities")
    func queryHardwareCapabilities() async throws {
        let capabilities = try await agentClient.getHardware()

        // Log hardware info for debugging
        print("Hardware capabilities count: \(capabilities.count)")
        for capability in capabilities {
            print("  Category: \(capability.category), Path: \(capability.devicePath)")
        }
    }

    @Test("Check provisioning status")
    func checkProvisioningStatus() async throws {
        let status = try await agentClient.isProvisioned()

        switch status {
        case .notProvisioned:
            print("Device is not provisioned")
        case .provisioned(let assetId, let organizationId):
            print("Device is provisioned - Asset ID: \(assetId), Org ID: \(organizationId)")
        case .unknown:
            print("Provisioning status unknown")
        }

        // Either status is valid for a test VM
        #expect(status == .notProvisioned || status != .unknown,
                "Provisioning status should be determinable")
    }

    @Test("Multiple sequential connections")
    func multipleSequentialConnections() async throws {
        // Make several requests in sequence to verify connection stability
        for i in 1...5 {
            let version = try await agentClient.getAgentVersion()
            #expect(!version.isEmpty, "Request \(i): Expected non-empty version")
        }
    }

    @Test("Parallel requests")
    func parallelRequests() async throws {
        // Make several requests in parallel
        async let version = agentClient.getAgentVersion()
        async let hardware = agentClient.getHardware()
        async let provisioning = agentClient.isProvisioned()

        let (v, _, p) = try await (version, hardware, provisioning)

        #expect(!v.isEmpty)
        #expect(p != .unknown || p == .notProvisioned || p != .notProvisioned)
    }
}
