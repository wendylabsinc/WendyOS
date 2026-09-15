import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("HardwareInventory parsing")
struct HardwareInventoryParsingTests {
    @Test("device metadata distinguishes Metal capability from legacy absence")
    func metalMetadata() async throws {
        let service = AgentService(
            gpuDiscovery: GPUDiscovery(devices: {
                [.init(name: "Apple GPU", vendor: "apple", computeBackends: ["metal"])]
            }),
            reportedVersion: { "test" }
        )
        let response = try await service.getAgentVersion(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_GetAgentVersionRequest()
            ),
            context: makeHardwareContext()
        ).message
        #expect(response.hasGpu_p)
        #expect(response.gpuVendor == "apple")
        #expect(response.gpuCapabilities.map(\.vendor) == ["apple"])
        #expect(response.gpuCapabilities.map(\.computeBackends) == [["metal"]])
    }

    @Test("advertises the native-process feature without a version suffix")
    func advertisesNativeProcessFeature() async throws {
        let service = AgentService(
            gpuDiscovery: GPUDiscovery(devices: { [] }),
            reportedVersion: { "test" }
        )
        let response = try await service.getAgentVersion(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_GetAgentVersionRequest()
            ),
            context: makeHardwareContext()
        ).message
        #expect(response.featureset.contains("native-process"))
        #expect(!response.featureset.contains { $0.hasSuffix("-v1") })
    }

    @Test("device metadata lists no GPU entries when Metal finds no device")
    func noMetalDevices() async throws {
        let service = AgentService(
            gpuDiscovery: GPUDiscovery(devices: { [] }),
            reportedVersion: { "test" }
        )
        let response = try await service.getAgentVersion(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_GetAgentVersionRequest()
            ),
            context: makeHardwareContext()
        ).message
        #expect(!response.hasGpu_p)
        #expect(response.gpuCapabilities.isEmpty)
    }

    @Test("parses GPU from SPDisplaysDataType")
    func parsesGPU() {
        let json = Data(
            #"{"SPDisplaysDataType":[{"_name":"Apple M2","spdisplays_vendor":"Apple (0x106b)"}]}"#
                .utf8
        )
        let caps = HardwareInventory.parseSystemProfiler(
            displays: json,
            usb: nil,
            camera: nil,
            audio: nil,
            storage: nil
        )
        #expect(caps.contains { $0.category == "gpu" && $0.description == "Apple M2" })
    }

    @Test("parses leaf USB devices, skipping controllers")
    func parsesUSB() {
        let json = Data(
            #"""
            {"SPUSBDataType":[{"_name":"USB Controller","_items":[
              {"_name":"Keyboard","vendor_id":"0x05ac","product_id":"0x0250","location_id":"0x14100000"}
            ]}]}
            """#.utf8
        )
        let caps = HardwareInventory.parseSystemProfiler(
            displays: nil,
            usb: json,
            camera: nil,
            audio: nil,
            storage: nil
        )
        let usb = caps.filter { $0.category == "usb" }
        #expect(usb.count == 1)
        #expect(usb.first?.description == "Keyboard")
        #expect(usb.first?.properties["vendor_id"] == "0x05ac")
    }

    @Test("parses storage size")
    func parsesStorage() {
        let json = Data(
            #"{"SPStorageDataType":[{"_name":"Macintosh HD","size_in_bytes":994662584320,"mount_point":"/","bsd_name":"disk3s1"}]}"#
                .utf8
        )
        let caps = HardwareInventory.parseSystemProfiler(
            displays: nil,
            usb: nil,
            camera: nil,
            audio: nil,
            storage: json
        )
        let storage = caps.first { $0.category == "storage" }
        #expect(storage?.devicePath == "disk3s1")
        #expect(storage?.properties["size_in_bytes"] == "994662584320")
    }

    @Test("empty input yields no capabilities")
    func emptyInput() {
        let caps = HardwareInventory.parseSystemProfiler(
            displays: nil,
            usb: nil,
            camera: nil,
            audio: nil,
            storage: nil
        )
        #expect(caps.isEmpty)
    }
}

private struct FakeHardwareDiscoverer: HardwareDiscovering {
    var result: [HardwareCapability]
    var lastFilter: LockedFilter = .init()

    func discover(categoryFilter: String?) async throws -> [HardwareCapability] {
        lastFilter.value = categoryFilter
        return result
    }

    final class LockedFilter: @unchecked Sendable {
        var value: String?
    }
}

@Suite("listHardwareCapabilities adapter")
struct HardwareCapabilitiesAdapterTests {
    @Test("maps provider capabilities to proto and forwards filter")
    func mapsAndForwardsFilter() async throws {
        let fake = FakeHardwareDiscoverer(result: [
            HardwareCapability(
                category: "gpu",
                devicePath: "bus0",
                description: "Apple M2",
                properties: ["spdisplays_vendor": "Apple"]
            )
        ])
        let service = AgentService(hardware: fake)

        var request = Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest()
        request.categoryFilter = "gpu"

        let response = try await service.listHardwareCapabilities(
            request: ServerRequest(metadata: [:], message: request),
            context: makeHardwareContext()
        )

        let message = try response.message
        #expect(fake.lastFilter.value == "gpu")
        #expect(message.capabilities.count == 1)
        let cap = message.capabilities[0]
        #expect(cap.category == "gpu")
        #expect(cap.devicePath == "bus0")
        #expect(cap.description_p == "Apple M2")
        #expect(cap.properties["spdisplays_vendor"] == "Apple")
    }
}

private func makeHardwareContext() -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyAgentService",
            method: "ListHardwareCapabilities"
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}
