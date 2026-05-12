import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("Unsupported RPC contract")
struct UnsupportedRPCTests {
    @Test("agent service unsupported RPCs use contextual macOS messages")
    func agentServiceUnsupportedRPCs() async {
        let service = AgentService()

        await assertUnsupportedCases([
            (
                "RunContainer",
                "Streaming container upload and execution is currently not supported on macOS.",
                {
                    _ = try await service.runContainer(
                        request: makeStreamingRequest(
                            Wendy_Agent_Services_V1_RunContainerRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "RunContainer"
                        )
                    )
                }
            ),
            (
                "UpdateAgent",
                "Updating the agent is currently not supported on macOS.",
                {
                    _ = try await service.updateAgent(
                        request: makeStreamingRequest(
                            Wendy_Agent_Services_V1_UpdateAgentRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "UpdateAgent"
                        )
                    )
                }
            ),
            (
                "ListWiFiNetworks",
                "Wi-Fi network scanning is currently not supported on macOS.",
                {
                    _ = try await service.listWiFiNetworks(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListWiFiNetworksRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ListWiFiNetworks"
                        )
                    )
                }
            ),
            (
                "ConnectToWiFi",
                "Connecting to Wi-Fi networks is currently not supported on macOS.",
                {
                    _ = try await service.connectToWiFi(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ConnectToWiFiRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ConnectToWiFi"
                        )
                    )
                }
            ),
            (
                "GetWiFiStatus",
                "Wi-Fi status reporting is currently not supported on macOS.",
                {
                    _ = try await service.getWiFiStatus(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_GetWiFiStatusRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "GetWiFiStatus"
                        )
                    )
                }
            ),
            (
                "DisconnectWiFi",
                "Disconnecting from Wi-Fi networks is currently not supported on macOS.",
                {
                    _ = try await service.disconnectWiFi(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_DisconnectWiFiRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "DisconnectWiFi"
                        )
                    )
                }
            ),
            (
                "ListKnownWiFiNetworks",
                "Listing saved Wi-Fi networks is currently not supported on macOS.",
                {
                    _ = try await service.listKnownWiFiNetworks(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListKnownWiFiNetworksRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ListKnownWiFiNetworks"
                        )
                    )
                }
            ),
            (
                "SetWiFiNetworkPriority",
                "Wi-Fi network priority management is currently not supported on macOS.",
                {
                    _ = try await service.setWiFiNetworkPriority(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_SetWiFiNetworkPriorityRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "SetWiFiNetworkPriority"
                        )
                    )
                }
            ),
            (
                "ReorderKnownWiFiNetworks",
                "Reordering saved Wi-Fi networks is currently not supported on macOS.",
                {
                    _ = try await service.reorderKnownWiFiNetworks(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ReorderKnownWiFiNetworks"
                        )
                    )
                }
            ),
            (
                "ForgetWiFiNetwork",
                "Removing saved Wi-Fi networks is currently not supported on macOS.",
                {
                    _ = try await service.forgetWiFiNetwork(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ForgetWiFiNetworkRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ForgetWiFiNetwork"
                        )
                    )
                }
            ),
            (
                "ListHardwareCapabilities",
                "Hardware capability discovery is currently not supported on macOS.",
                {
                    _ = try await service.listHardwareCapabilities(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ListHardwareCapabilities"
                        )
                    )
                }
            ),
            (
                "ScanBluetoothPeripherals",
                "Bluetooth scanning is currently not supported on macOS.",
                {
                    _ = try await service.scanBluetoothPeripherals(
                        request: makeStreamingRequest(
                            Wendy_Agent_Services_V1_ScanBluetoothPeripheralsRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ScanBluetoothPeripherals"
                        )
                    )
                }
            ),
            (
                "ConnectBluetoothPeripheral",
                "Connecting Bluetooth peripherals is currently not supported on macOS.",
                {
                    _ = try await service.connectBluetoothPeripheral(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ConnectBluetoothPeripheralRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ConnectBluetoothPeripheral"
                        )
                    )
                }
            ),
            (
                "DisconnectBluetoothPeripheral",
                "Disconnecting Bluetooth peripherals is currently not supported on macOS.",
                {
                    _ = try await service.disconnectBluetoothPeripheral(
                        request: ServerRequest(
                            metadata: [:],
                            message:
                                Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "DisconnectBluetoothPeripheral"
                        )
                    )
                }
            ),
            (
                "ForgetBluetoothPeripheral",
                "Forgetting Bluetooth peripherals is currently not supported on macOS.",
                {
                    _ = try await service.forgetBluetoothPeripheral(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ForgetBluetoothPeripheralRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "ForgetBluetoothPeripheral"
                        )
                    )
                }
            ),
            (
                "UpdateOS",
                "This setup cannot be updated with wendy os update. Use this machine’s normal OS update tools instead. To use WendyOS OTA updates, install WendyOS on supported hardware with wendy os install.",
                {
                    _ = try await service.updateOS(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_UpdateOSRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAgentService",
                            method: "UpdateOS"
                        )
                    )
                }
            ),
        ])
    }

    @Test("audio service unsupported RPCs use contextual macOS messages")
    func audioServiceUnsupportedRPCs() async {
        let service = AudioService()

        await assertUnsupportedCases([
            (
                "ListAudioDevices",
                "Listing audio devices is currently not supported on macOS.",
                {
                    _ = try await service.listAudioDevices(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListAudioDevicesRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAudioService",
                            method: "ListAudioDevices"
                        )
                    )
                }
            ),
            (
                "SetDefaultAudioDevice",
                "Changing the default audio device is currently not supported on macOS.",
                {
                    _ = try await service.setDefaultAudioDevice(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_SetDefaultAudioDeviceRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAudioService",
                            method: "SetDefaultAudioDevice"
                        )
                    )
                }
            ),
            (
                "StreamAudioLevels",
                "Streaming audio levels is currently not supported on macOS.",
                {
                    _ = try await service.streamAudioLevels(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_StreamAudioLevelsRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAudioService",
                            method: "StreamAudioLevels"
                        )
                    )
                }
            ),
            (
                "StreamAudio",
                "Streaming audio is currently not supported on macOS.",
                {
                    _ = try await service.streamAudio(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_StreamAudioRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyAudioService",
                            method: "StreamAudio"
                        )
                    )
                }
            ),
        ])
    }

    @Test("container service placeholder RPCs use contextual macOS messages")
    func containerServiceUnsupportedRPCs() async {
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false"
        )

        await assertUnsupportedCases([
            (
                "AttachContainer",
                "Linux container attach is currently not supported on macOS.",
                {
                    _ = try await service.attachContainer(
                        request: makeStreamingRequest(
                            Wendy_Agent_Services_V1_AttachContainerRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "AttachContainer"
                        )
                    )
                }
            ),
            (
                "ListVolumes",
                "Container volume management is currently not supported on macOS.",
                {
                    _ = try await service.listVolumes(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListVolumesRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "ListVolumes"
                        )
                    )
                }
            ),
            (
                "RemoveVolume",
                "Removing container volumes is currently not supported on macOS.",
                {
                    _ = try await service.removeVolume(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_RemoveVolumeRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "RemoveVolume"
                        )
                    )
                }
            ),
            (
                "ListLayers",
                "Container layer listing is currently not supported on macOS.",
                {
                    _ = try await service.listLayers(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_ListLayersRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "ListLayers"
                        )
                    )
                }
            ),
            (
                "CreateContainerWithProgress",
                "Container creation progress streaming is currently not supported on macOS.",
                {
                    _ = try await service.createContainerWithProgress(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_CreateContainerRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "CreateContainerWithProgress"
                        )
                    )
                }
            ),
            (
                "RunContainer",
                "Legacy container streaming execution is currently not supported on macOS. Use the native app lifecycle RPCs instead when applicable.",
                {
                    _ = try await service.runContainer(
                        request: ServerRequest(
                            metadata: [:],
                            message: Wendy_Agent_Services_V1_RunContainerLayersRequest()
                        ),
                        context: makeServerContext(
                            service: "wendy.agent.services.v1.WendyContainerService",
                            method: "RunContainer"
                        )
                    )
                }
            ),
        ])
    }

}

private typealias UnsupportedCase = (
    name: String,
    message: String,
    call: @Sendable () async throws -> Void
)

private func assertUnsupportedCases(_ cases: [UnsupportedCase]) async {
    for unsupportedCase in cases {
        await assertUnsupported(
            unsupportedCase.name,
            expectedMessage: unsupportedCase.message,
            unsupportedCase.call
        )
    }
}

private func assertUnsupported(
    _ name: String,
    expectedMessage: String,
    _ call: @escaping @Sendable () async throws -> Void
) async {
    do {
        try await call()
        Issue.record("Expected \(name) to be unsupported")
    } catch let error as RPCError {
        #expect(error.code == .unimplemented)
        #expect(error.message == expectedMessage)
        #expect(
            error.message.contains("not supported on macOS")
                || error.message.contains("cannot be updated with wendy os update")
        )
    } catch {
        Issue.record("Expected \(name) to throw RPCError, got \(error)")
    }
}

private func makeServerContext(service: String, method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(fullyQualifiedService: service, method: method),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}

private func makeStreamingRequest<Message: Sendable>(
    _ message: Message
) -> StreamingServerRequest<Message> {
    StreamingServerRequest(
        metadata: [:],
        messages: RPCAsyncSequence(
            wrapping: AsyncThrowingStream<Message, any Error> { continuation in
                continuation.yield(message)
                continuation.finish()
            }
        )
    )
}
