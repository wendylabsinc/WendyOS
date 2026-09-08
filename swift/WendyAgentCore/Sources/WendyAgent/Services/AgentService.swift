import Foundation
import GRPCCore
import Logging
import WendyAgentGRPC

struct AgentService: Wendy_Agent_Services_V1_WendyAgentService.ServiceProtocol {
    var hardware: any HardwareDiscovering = HardwareInventory()
    var hostname: any HostnameSetting = ScutilHostname()
    var wifi: any WiFiManaging = WiFiController()
    var bluetooth: any BluetoothManaging = BluetoothScanner()
    /// Digest of the executable mapped by this process, captured at startup.
    var binarySHA256: String = ""
    /// Serializes agent self-updates. Held by `WendyAgent` (not created here)
    /// so the same lock survives the service rebuilds that provisioning
    /// transitions perform.
    var updateLock: AgentUpdateLock = AgentUpdateLock()
    var updateDependencies: AgentUpdateSession.Dependencies = .init(
        relauncher: AgentRelauncher(terminate: nil)
    )

    // A `static` logger, not a stored property: a private stored property
    // would make the synthesized memberwise initializer private, and callers
    // (WendyAgent, tests) construct `AgentService` through it.
    private static let updateLogger = Logger(label: "sh.wendy.agent.update")

    func runContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_RunContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerResponse> {
        throw RPCError(
            code: .unimplemented,
            message:
                "Streaming container upload and execution is currently not supported by Wendy Agent for Mac."
        )
    }

    func updateAgent(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateAgentResponse> {
        guard await self.updateLock.tryAcquire() else {
            throw RPCError(code: .failedPrecondition, message: "an update is already in progress")
        }

        // StreamingServerRequest and its RPCAsyncSequence are both Sendable,
        // so `messages` can be captured directly in the @Sendable producer.
        let messages = request.messages
        let updateLock = self.updateLock
        // The commit hook is wired here rather than by whoever built the
        // dependencies: this is the only place that holds both the lock and
        // the session. Once the session commits, the lock must stop being
        // stealable (see `AgentUpdateLock`) — the process is exiting, and a
        // steal at that point could install a second bundle over the one just
        // written.
        let dependencies: AgentUpdateSession.Dependencies = {
            var dependencies = self.updateDependencies
            dependencies.onCommitted = { await updateLock.markCommitted() }
            return dependencies
        }()
        let logger = Self.updateLogger

        return StreamingServerResponse { writer in
            do {
                try await AgentUpdateSession.run(
                    messages: messages,
                    writeResponse: { try await writer.write($0) },
                    deps: dependencies,
                    logger: logger
                )
            } catch {
                // Only released on failure: a committed update exits the
                // process, and a retry against a committed-but-not-yet-dead
                // agent must still see "an update is already in progress".
                await updateLock.release()
                throw error
            }
            return Metadata()
        }
    }

    func getAgentVersion(
        request: ServerRequest<Wendy_Agent_Services_V1_GetAgentVersionRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetAgentVersionResponse> {
        let osVersion = ProcessInfo.processInfo.operatingSystemVersion
        var response = Wendy_Agent_Services_V1_GetAgentVersionResponse()
        response.version = WendyAgent.version
        response.os = "darwin"
        response.osVersion =
            "\(osVersion.majorVersion).\(osVersion.minorVersion).\(osVersion.patchVersion)"
        #if arch(arm64)
            response.cpuArchitecture = "arm64"
        #elseif arch(x86_64)
            response.cpuArchitecture = "amd64"
        #endif
        response.memTotalBytes = Int64(clamping: ProcessInfo.processInfo.physicalMemory)
        response.cpuCount = UInt32(clamping: ProcessInfo.processInfo.activeProcessorCount)
        response.binarySha256 = self.binarySHA256
        return ServerResponse(message: response)
    }

    func listWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ListWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListWiFiNetworksResponse> {
        let results: [WiFiScanResult]
        do {
            results = try await wifi.scan()
        } catch {
            throw RPCError(code: .internalError, message: "\(error)")
        }
        var response = Wendy_Agent_Services_V1_ListWiFiNetworksResponse()
        response.networks = results.map { result in
            var network = Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork()
            network.ssid = result.ssid
            network.signalStrength = result.signalStrength
            network.rssiDbm = Int32(clamping: result.rssiDbm)
            network.security = Self.protoSecurity(result.security)
            network.isKnown = result.isKnown
            network.isConnected = result.isConnected
            return network
        }
        return ServerResponse(message: response)
    }

    func connectToWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectToWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectToWiFiResponse> {
        let message = request.message
        let security = message.hasSecurity ? Self.modelSecurity(message.security) : nil
        let result = await wifi.connect(
            ssid: message.ssid,
            password: message.password,
            security: security,
            hidden: message.hasHidden ? message.hidden : false
        )
        var response = Wendy_Agent_Services_V1_ConnectToWiFiResponse()
        response.success = result.success
        if let errorMessage = result.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func getWiFiStatus(
        request: ServerRequest<Wendy_Agent_Services_V1_GetWiFiStatusRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetWiFiStatusResponse> {
        let status = await wifi.status()
        var response = Wendy_Agent_Services_V1_GetWiFiStatusResponse()
        response.connected = status.connected
        if let ssid = status.ssid { response.ssid = ssid }
        if let errorMessage = status.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func disconnectWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectWiFiResponse> {
        let result = await wifi.disconnect()
        var response = Wendy_Agent_Services_V1_DisconnectWiFiResponse()
        response.success = result.success
        if let errorMessage = result.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func listKnownWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ListKnownWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListKnownWiFiNetworksResponse> {
        let known = await wifi.knownNetworks()
        var response = Wendy_Agent_Services_V1_ListKnownWiFiNetworksResponse()
        response.networks = known.map { network in
            var proto = Wendy_Agent_Services_V1_ListKnownWiFiNetworksResponse.KnownWiFiNetwork()
            proto.ssid = network.ssid
            proto.priority = network.priority
            proto.security = Self.protoSecurity(network.security)
            return proto
        }
        return ServerResponse(message: response)
    }

    func setWiFiNetworkPriority(
        request: ServerRequest<Wendy_Agent_Services_V1_SetWiFiNetworkPriorityRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetWiFiNetworkPriorityResponse> {
        let result = await wifi.setPriority(
            ssid: request.message.ssid,
            priority: request.message.priority
        )
        var response = Wendy_Agent_Services_V1_SetWiFiNetworkPriorityResponse()
        response.success = result.success
        if let errorMessage = result.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func reorderKnownWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksResponse> {
        let result = await wifi.reorder(ssids: request.message.orderSsids)
        var response = Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksResponse()
        response.success = result.success
        if let errorMessage = result.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func forgetWiFiNetwork(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetWiFiNetworkRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetWiFiNetworkResponse> {
        let result = await wifi.forget(ssid: request.message.ssid)
        var response = Wendy_Agent_Services_V1_ForgetWiFiNetworkResponse()
        response.success = result.success
        if let errorMessage = result.errorMessage { response.errorMessage = errorMessage }
        return ServerResponse(message: response)
    }

    func listHardwareCapabilities(
        request: ServerRequest<Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse> {
        let filter = request.message.hasCategoryFilter ? request.message.categoryFilter : nil
        let capabilities = try await hardware.discover(categoryFilter: filter)

        var response = Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse()
        response.capabilities = capabilities.map { capability in
            var proto =
                Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse.HardwareCapability()
            proto.category = capability.category
            proto.devicePath = capability.devicePath
            proto.description_p = capability.description
            proto.properties = capability.properties
            return proto
        }
        return ServerResponse(message: response)
    }

    func scanBluetoothPeripherals(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_ScanBluetoothPeripheralsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<
        Wendy_Agent_Services_V1_ScanBluetoothPeripheralsResponse
    > {
        let stream = bluetooth.scan()
        return StreamingServerResponse { writer in
            for await peripheral in stream {
                var discovered = Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral()
                discovered.name = peripheral.name
                discovered.address = peripheral.address
                discovered.rssi = peripheral.rssi
                discovered.deviceType = peripheral.deviceType
                discovered.paired = peripheral.paired
                discovered.connected = peripheral.connected
                discovered.trusted = peripheral.trusted

                var response = Wendy_Agent_Services_V1_ScanBluetoothPeripheralsResponse()
                response.discoveredDevices = [discovered]
                try await writer.write(response)
            }
            return Metadata()
        }
    }

    func connectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralResponse> {
        let result = await bluetooth.connect(address: request.message.address)
        guard result.success else {
            throw RPCError(
                code: .internalError,
                message: result.errorMessage ?? "Failed to connect Bluetooth peripheral."
            )
        }
        return ServerResponse(message: Wendy_Agent_Services_V1_ConnectBluetoothPeripheralResponse())
    }

    func disconnectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralResponse>
    {
        let result = await bluetooth.disconnect(address: request.message.address)
        guard result.success else {
            throw RPCError(
                code: .internalError,
                message: result.errorMessage ?? "Failed to disconnect Bluetooth peripheral."
            )
        }
        return ServerResponse(
            message: Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralResponse()
        )
    }

    func forgetBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralResponse> {
        throw RPCError(
            code: .unimplemented,
            message:
                "Forgetting Bluetooth peripherals is not supported on macOS: CoreBluetooth is BLE-only and does not expose classic pairing state to remove."
        )
    }

    func dumpKernelLog(
        request: ServerRequest<Wendy_Agent_Services_V1_DumpKernelLogRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_DumpKernelLogResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Dumping the kernel log is currently not supported by Wendy Agent for Mac."
        )
    }

    func updateOS(
        request: ServerRequest<Wendy_Agent_Services_V1_UpdateOSRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateOSResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "This setup cannot be updated with wendy os update. "
                + "Use this machine’s normal OS update tools instead. "
                + "To use WendyOS OTA updates, install WendyOS on supported hardware "
                + "with wendy os install."
        )
    }

    func getOSUpdateStatus(
        request: ServerRequest<Wendy_Agent_Services_V1_GetOSUpdateStatusRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetOSUpdateStatusResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "OS update status is currently not supported by Wendy Agent for Mac."
        )
    }

    func setHostname(
        request: ServerRequest<Wendy_Agent_Services_V1_SetHostnameRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetHostnameResponse> {
        let requested = request.message.hostname
        do {
            try await hostname.setHostname(requested)
        } catch let error as HostnameError {
            throw RPCError(code: .permissionDenied, message: "\(error)")
        } catch {
            throw RPCError(
                code: .permissionDenied,
                message: "Failed to set hostname: \(error)"
            )
        }

        var response = Wendy_Agent_Services_V1_SetHostnameResponse()
        response.hostname = requested.trimmingCharacters(in: .whitespacesAndNewlines)
        return ServerResponse(message: response)
    }

    // MARK: - Wi-Fi security mapping

    static func protoSecurity(_ security: WiFiSecurity) -> Wendy_Agent_Services_V1_WiFiSecurityType
    {
        switch security {
        case .unspecified: return .unspecified
        case .open: return .open
        case .wep: return .wep
        case .wpaPersonal: return .wpaPsk
        case .wpa2Personal: return .wpa2Psk
        case .wpa3Personal: return .wpa3Sae
        case .wpa2Enterprise: return .wpa2Enterprise
        }
    }

    static func modelSecurity(_ security: Wendy_Agent_Services_V1_WiFiSecurityType) -> WiFiSecurity
    {
        switch security {
        case .unspecified: return .unspecified
        case .open: return .open
        case .wep: return .wep
        case .wpaPsk: return .wpaPersonal
        case .wpa2Psk: return .wpa2Personal
        case .wpa3Sae: return .wpa3Personal
        case .wpa2Enterprise: return .wpa2Enterprise
        case .UNRECOGNIZED: return .unspecified
        }
    }
}
