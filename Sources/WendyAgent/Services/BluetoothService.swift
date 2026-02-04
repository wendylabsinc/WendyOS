import Bluetooth
import ContainerdGRPC
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import ServiceLifecycle
import SwiftProtobuf
import WendyAgentGRPC
import WendyShared

#if canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

/// Service that exposes the WendyOS agent over Bluetooth Low Energy
/// This allows CLI clients to discover and communicate with devices without network connectivity
actor BluetoothService: Service {
    // BLE advertising packet size constraints
    private static let legacyAdvertisingMaxBytes = 31
    private static let uuidBytes = 16
    private static let flagsFieldBytes = 3
    private static let localNameHeaderBytes = 2

    private let logger = Logger(label: "BluetoothService")
    private let networkManagerFactory: NetworkConnectionManagerFactory
    private let configuration: WendyAgentConfiguration
    private var peripheralManager: PeripheralManager?

    init() {
        let uid = String(getuid())
        self.networkManagerFactory = NetworkConnectionManagerFactory(uid: uid)
        self.configuration = WendyAgentConfiguration.fromEnvironment()
    }

    func run() async throws {
        logger.debug("Starting Bluetooth service")

        enum Phase: String {
            case initialization = "initialization"
            case waitingForReady = "waiting_for_bluetooth_ready"
            case startingAdvertising = "starting_advertising"
            case handlingConnections = "handling_connections"
        }
        var currentPhase: Phase = .initialization

        do {
            logger.debug("Creating PeripheralManager...")
            let manager = PeripheralManager()
            self.peripheralManager = manager
            logger.debug("PeripheralManager created successfully")

            // Wait for Bluetooth to be ready
            currentPhase = .waitingForReady
            logger.debug("Phase: waiting for Bluetooth to be ready")
            try await waitForBluetoothReady(manager)

            // Start advertising - failures are non-fatal since L2CAP can still work
            // if the device was previously discovered or address is known
            currentPhase = .startingAdvertising
            logger.debug("Phase: starting Bluetooth advertising")
            do {
                try await startAdvertising(manager)
            } catch {
                logger.warning(
                    "Advertising failed, but continuing with L2CAP listener",
                    metadata: [
                        "error": "\(error)",
                        "hint":
                            "Device may not be discoverable, but direct connections may still work",
                    ]
                )
            }

            // Handle incoming L2CAP connections
            currentPhase = .handlingConnections
            logger.debug("Phase: handling incoming L2CAP connections")
            try await handleConnections(manager)
        } catch is CancellationError {
            logger.debug(
                "Bluetooth service cancelled",
                metadata: ["phase": "\(currentPhase.rawValue)"]
            )
        } catch {
            // Log the error but don't crash the entire agent
            // Bluetooth is optional functionality
            logger.warning(
                "Bluetooth service failed (agent continues without Bluetooth)",
                metadata: [
                    "error": "\(error)",
                    "errorType": "\(type(of: error))",
                    "errorDescription": "\((error as? LocalizedError)?.errorDescription ?? "N/A")",
                    "phase": "\(currentPhase.rawValue)",
                ]
            )

            // Wait for graceful shutdown instead of throwing
            // This prevents the error from bringing down the entire service group
            try await withGracefulShutdownHandler {
                // Wait indefinitely until shutdown
                while !Task.isCancelled {
                    try await Task.sleep(for: .seconds(3600))
                }
            } onGracefulShutdown: {
                self.logger.debug("Bluetooth service (disabled) shutting down")
            }
        }
    }

    private func waitForBluetoothReady(_ manager: PeripheralManager) async throws {
        let maxWaitTime: Duration = .seconds(30)
        let startTime = ContinuousClock.now

        logger.debug("Checking initial Bluetooth state...")
        let initialState = await manager.state()
        logger.debug("Initial Bluetooth state", metadata: ["state": "\(initialState)"])

        switch initialState {
        case .poweredOn:
            logger.debug("Bluetooth already powered on")
            return
        case .poweredOff:
            logger.warning("Bluetooth is powered off")
            throw BluetoothServiceError.poweredOff
        case .unauthorized:
            logger.warning("Bluetooth access is not authorized")
            throw BluetoothServiceError.unauthorized
        case .unsupported:
            logger.warning("Bluetooth is not supported on this device")
            throw BluetoothServiceError.unsupported
        default:
            logger.debug("Bluetooth state not ready yet, waiting for updates...")
        }

        // Wait for state to change to poweredOn
        logger.debug("Starting to monitor Bluetooth state updates...")
        var stateUpdateCount = 0
        var lastState = initialState

        for await state in await manager.stateUpdates() {
            stateUpdateCount += 1
            lastState = state
            logger.debug(
                "Bluetooth state update",
                metadata: [
                    "state": "\(state)",
                    "updateNumber": "\(stateUpdateCount)",
                ]
            )

            switch state {
            case .poweredOn:
                logger.debug("Bluetooth is now powered on and ready")
                return
            case .poweredOff:
                throw BluetoothServiceError.poweredOff
            case .unauthorized:
                throw BluetoothServiceError.unauthorized
            case .unsupported:
                throw BluetoothServiceError.unsupported
            default:
                // Check timeout
                let elapsed = ContinuousClock.now - startTime
                if elapsed > maxWaitTime {
                    logger.warning(
                        "Timeout waiting for Bluetooth to be ready",
                        metadata: [
                            "elapsed": "\(elapsed)",
                            "lastState": "\(state)",
                            "stateUpdates": "\(stateUpdateCount)",
                        ]
                    )
                    throw BluetoothServiceError.timeout(
                        waitingFor: "poweredOn",
                        lastState: "\(state)"
                    )
                }
                continue
            }
        }

        // Stream ended without reaching poweredOn
        logger.warning(
            "Bluetooth state stream ended without definitive state - proceeding optimistically",
            metadata: [
                "totalUpdates": "\(stateUpdateCount)",
                "lastState": "\(lastState)",
            ]
        )
    }

    private func startAdvertising(_ manager: PeripheralManager) async throws {
        // Get hostname for device name - use gethostname() on Linux for reliability
        let hostname = getSystemHostname()
        let deviceName = extractDeviceName(from: hostname)
        logger.info(
            "Preparing advertisement",
            metadata: [
                "hostname": "\(hostname)",
                "advertisingName": "\(deviceName)",
            ]
        )

        // Convert string UUID to Foundation UUID
        guard let serviceUUID = UUID(uuidString: WendyBluetoothUUIDs.serviceUUID) else {
            logger.error(
                "Invalid service UUID configuration",
                metadata: [
                    "serviceUUID": "\(WendyBluetoothUUIDs.serviceUUID)"
                ]
            )
            throw BluetoothServiceError.invalidConfiguration
        }

        // deviceName was already extracted above for logging
        let shortName = deviceName

        // Calculate approximate advertising data size:
        // - Flags: flagsFieldBytes
        // - Complete Local Name: localNameHeaderBytes + name.utf8.count bytes
        // Total must be <= legacyAdvertisingMaxBytes for legacy advertising
        // If name is too long, truncate it
        let maxNameLength =
            Self.legacyAdvertisingMaxBytes - Self.flagsFieldBytes - Self.localNameHeaderBytes
            - Self.uuidBytes
        let advertisingName: String
        if shortName.utf8.count > maxNameLength {
            // Truncate to fit, ensuring we don't cut in the middle of a multi-byte character
            var truncated = shortName
            while truncated.utf8.count > maxNameLength && !truncated.isEmpty {
                truncated.removeLast()
            }
            advertisingName = truncated
            logger.debug(
                "Truncated advertising name to fit legacy limit",
                metadata: [
                    "original": "\(shortName)",
                    "truncated": "\(advertisingName)",
                    "originalBytes": "\(shortName.utf8.count)",
                    "truncatedBytes": "\(advertisingName.utf8.count)",
                ]
            )
        } else {
            advertisingName = shortName
        }

        let advertisementData = AdvertisementData(
            localName: advertisingName,
            serviceUUIDs: [BluetoothUUID(serviceUUID)]
        )

        logger.debug(
            "Starting Bluetooth advertising",
            metadata: [
                "advertisingName": "\(advertisingName)",
                "nameBytes": "\(advertisingName.utf8.count)",
                "serviceUUID": "\(serviceUUID) (not included in advertising)",
            ]
        )

        do {
            // Pass nil for scanResponseData - BlueZ doesn't properly support it
            // and will merge it into advertising data, pushing us over 31 bytes
            try await manager.startAdvertising(
                advertisingData: advertisementData,
                scanResponseData: nil,
                parameters: AdvertisingParameters()
            )
            logger.debug("Bluetooth advertising started successfully")
        } catch {
            logger.error(
                "Failed to start advertising",
                metadata: [
                    "error": "\(error)",
                    "errorType": "\(type(of: error))",
                ]
            )
            throw BluetoothServiceError.advertisingFailed(underlying: error)
        }
    }

    private func handleConnections(_ manager: PeripheralManager) async throws {
        logger.debug("Setting up L2CAP channel...")

        // Publish L2CAP channel for bidirectional communication
        let psm: L2CAPPSM
        do {
            logger.debug("Publishing L2CAP channel (requiresEncryption: false)")
            psm = try await manager.publishL2CAPChannel(
                parameters: L2CAPChannelParameters(requiresEncryption: false)
            )
            logger.debug(
                "L2CAP channel published successfully",
                metadata: ["psm": "\(psm.rawValue)"]
            )
        } catch {
            logger.error(
                "Failed to publish L2CAP channel",
                metadata: [
                    "error": "\(error)",
                    "errorType": "\(type(of: error))",
                ]
            )
            throw BluetoothServiceError.l2capPublishFailed(underlying: error)
        }

        // Skip PSM update for now - BlueZ has issues with stop/restart advertising
        // The CLI will use the default PSM (128)
        logger.debug(
            "Skipping PSM service data update (BlueZ compatibility)",
            metadata: ["psm": "\(psm.rawValue)"]
        )

        // Get the incoming channel stream
        logger.debug("Creating incoming L2CAP channel stream...")
        let incomingChannels: AsyncThrowingStream<any L2CAPChannel, Error>
        do {
            incomingChannels = try await manager.incomingL2CAPChannels(psm: psm)
            logger.debug(
                "Incoming L2CAP channel stream created, waiting for connections...",
                metadata: ["psm": "\(psm.rawValue)"]
            )
        } catch {
            logger.error(
                "Failed to get incoming L2CAP channels",
                metadata: [
                    "error": "\(error)",
                    "errorType": "\(type(of: error))",
                ]
            )
            throw error
        }

        try await withGracefulShutdownHandler {
            self.logger.debug("Entering connection accept loop...")
            var connectionCount = 0

            try await withThrowingDiscardingTaskGroup { group in
                for try await channel in incomingChannels {
                    connectionCount += 1
                    self.logger.debug(
                        "New Bluetooth connection established",
                        metadata: [
                            "connectionNumber": "\(connectionCount)"
                        ]
                    )

                    // Handle each connection in a child task
                    group.addTask {
                        await self.handleChannel(channel)
                    }
                }
            }

            self.logger.debug(
                "L2CAP channel stream ended",
                metadata: [
                    "totalConnections": "\(connectionCount)"
                ]
            )
        } onGracefulShutdown: {
            self.logger.debug("Bluetooth service shutting down")
            Task {
                await manager.stopAdvertising()
            }
        }
    }

    private func handleChannel(_ channel: any L2CAPChannel) async {
        logger.debug("Starting to handle Bluetooth channel...")
        var messagesReceived = 0
        var stream = channel.incoming().makeAsyncIterator()
        var buffer = ByteBuffer()

        do {
            while !Task.isCancelled {
                // Process complete messages from buffer using length-prefixed framing
                while let messageSlice = buffer.readLengthPrefixedSlice(
                    endianness: .big,
                    as: UInt16.self
                ) {
                    messagesReceived += 1

                    logger.debug(
                        "Received complete Bluetooth message",
                        metadata: [
                            "messageNumber": "\(messagesReceived)",
                            "messageSize": "\(messageSlice.readableBytes)",
                        ]
                    )

                    // Parse protobuf command and process
                    do {
                        let command = try Wendy_Agent_Services_V1_BluetoothCommand(
                            serializedBytes: Array(messageSlice.readableBytesView)
                        )
                        let response = await processCommand(command)
                        let responseData = try response.serializedData()
                        logger.debug(
                            "Sending response",
                            metadata: ["responseSize": "\(responseData.count)"]
                        )
                        try await sendLengthPrefixed(responseData, on: channel)
                    } catch {
                        logger.error(
                            "Failed to parse Bluetooth command",
                            metadata: ["error": "\(error)"]
                        )
                        var errorResponse = Wendy_Agent_Services_V1_BluetoothResponse()
                        errorResponse.error = Wendy_Agent_Services_V1_ErrorResponse()
                        errorResponse.error.message = "Invalid command format: \(error)"
                        if let responseData = try? errorResponse.serializedData() {
                            try await sendLengthPrefixed(responseData, on: channel)
                        }
                    }
                }

                // Reclaim memory from processed messages
                buffer.discardReadBytes()

                // Wait for more data
                // TODO: Add timeout using Swift 6.3's withTimeout
                guard let data = try await stream.next() else { break }
                buffer.writeData(data)
            }
        } catch {
            let errorString = String(describing: error)
            if messagesReceived > 0 && errorString.contains("socket closed") {
                logger.debug(
                    "Client disconnected after request/response",
                    metadata: ["messagesProcessed": "\(messagesReceived)"]
                )
            } else {
                logger.error(
                    "Error handling Bluetooth channel",
                    metadata: [
                        "error": "\(error)",
                        "errorType": "\(type(of: error))",
                        "messagesProcessed": "\(messagesReceived)",
                    ]
                )
            }
        }

        logger.debug(
            "Bluetooth connection closed",
            metadata: ["totalMessagesProcessed": "\(messagesReceived)"]
        )
    }

    private func sendLengthPrefixed(_ data: Data, on channel: any L2CAPChannel) async throws {
        var buffer = ByteBuffer()
        try buffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { buffer in
            buffer.writeData(data)
        }
        try await channel.send(Data(buffer.readableBytesView))
    }

    private func processCommand(
        _ command: Wendy_Agent_Services_V1_BluetoothCommand
    ) async -> Wendy_Agent_Services_V1_BluetoothResponse {
        var response = Wendy_Agent_Services_V1_BluetoothResponse()

        do {
            switch command.command {
            case .wifiList:
                response.wifiList = try await handleWifiList()
            case .wifiConnect(let cmd):
                response.wifiConnect = try await handleWifiConnect(
                    ssid: cmd.ssid,
                    password: cmd.password
                )
            case .wifiStatus:
                response.wifiStatus = try await handleWifiStatus()
            case .wifiDisconnect:
                response.wifiDisconnect = try await handleWifiDisconnect()
            case .appsList:
                response.appsList = try await handleAppsList()
            case .appsStop(let cmd):
                response.appsStop = try await handleAppsStop(appName: cmd.appName)
            case .appsRemove(let cmd):
                response.appsRemove = try await handleAppsRemove(
                    appName: cmd.appName,
                    purgeImage: cmd.purgeImage
                )
            case .agentVersion:
                response.agentVersion = Wendy_Agent_Services_V1_AgentVersionResponse()
                response.agentVersion.version = Version.current
            case .hardwareList:
                response.hardwareList = try await handleHardwareList()
            case .none:
                response.error = Wendy_Agent_Services_V1_ErrorResponse()
                response.error.message = "Unknown command"
            }
        } catch {
            logger.error("Error processing Bluetooth command", metadata: ["error": "\(error)"])
            response.error = Wendy_Agent_Services_V1_ErrorResponse()
            response.error.message = error.localizedDescription
        }

        return response
    }

    // MARK: - WiFi Handlers

    private func handleWifiList() async throws -> Wendy_Agent_Services_V1_WifiListResponse {
        logger.debug("Bluetooth: Listing WiFi networks")

        let networkManager = try await networkManagerFactory.createNetworkManager(
            preference: configuration.networkManagerPreference
        )
        let networks = try await networkManager.listWiFiNetworks()

        var response = Wendy_Agent_Services_V1_WifiListResponse()
        response.networks = networks.map { network in
            var info = Wendy_Agent_Services_V1_WifiNetworkInfo()
            info.ssid = network.ssid
            if let signal = network.signalStrength {
                info.signalStrength = Int32(signal)
            }
            return info
        }

        return response
    }

    private func handleWifiConnect(
        ssid: String,
        password: String
    ) async throws -> Wendy_Agent_Services_V1_WifiConnectResponse {
        logger.debug("Bluetooth: Connecting to WiFi", metadata: ["ssid": "\(ssid)"])

        var response = Wendy_Agent_Services_V1_WifiConnectResponse()

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            try await networkManager.setupWiFi(ssid: ssid, password: password)

            if let connection = try await networkManager.getCurrentConnection() {
                logger.debug("Connected to WiFi", metadata: ["ssid": "\(connection.ssid)"])
                response.success = true
            } else {
                response.success = false
                response.errorMessage = "Connection failed"
            }
        } catch {
            response.success = false
            response.errorMessage = error.localizedDescription
        }

        return response
    }

    private func handleWifiStatus() async throws -> Wendy_Agent_Services_V1_WifiStatusResponse {
        logger.debug("Bluetooth: Getting WiFi status")

        var response = Wendy_Agent_Services_V1_WifiStatusResponse()

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            let connection = try await networkManager.getCurrentConnection()

            if let connection {
                response.connected = true
                response.ssid = connection.ssid
            } else {
                response.connected = false
            }
        } catch {
            response.connected = false
            response.errorMessage = error.localizedDescription
        }

        return response
    }

    private func handleWifiDisconnect() async throws
        -> Wendy_Agent_Services_V1_WifiDisconnectResponse
    {
        logger.debug("Bluetooth: Disconnecting from WiFi")

        var response = Wendy_Agent_Services_V1_WifiDisconnectResponse()

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )

            if try await networkManager.getCurrentConnection() != nil {
                let success = try await networkManager.disconnectFromNetwork()
                response.success = success
            } else {
                response.success = true
            }
        } catch NetworkConnectionError.noActiveConnection {
            response.success = true
        } catch {
            response.success = false
            response.errorMessage = error.localizedDescription
        }

        return response
    }

    // MARK: - Apps Handlers

    private func handleAppsList() async throws -> Wendy_Agent_Services_V1_AppsListResponse {
        logger.debug("Bluetooth: Listing apps")

        let (containers, tasks) = try await Containerd.withClient { client in
            let tasks = try await client.listTasks()
            let containers = try await client.listContainers()
            return (containers, tasks)
        }

        var response = Wendy_Agent_Services_V1_AppsListResponse()

        for container in containers {
            // Only include Wendy-managed containers
            guard let version = container.labels["sh.wendy/app.version"] else {
                continue
            }

            let task = tasks.first(where: { $0.id == container.id })
            let state: String
            switch task?.status {
            case .running:
                state = "Running"
            case .stopped, .none:
                state = "Stopped"
            default:
                state = "Unknown"
            }

            let failureCount =
                Int32(container.labels["sh.wendy/failure.count"] ?? "0") ?? 0

            var appInfo = Wendy_Agent_Services_V1_AppInfo()
            appInfo.appName = container.id
            appInfo.appVersion = version
            appInfo.state = state
            appInfo.failureCount = failureCount
            response.apps.append(appInfo)
        }

        return response
    }

    private func handleAppsStop(
        appName: String
    ) async throws
        -> Wendy_Agent_Services_V1_AppsStopResponse
    {
        logger.debug("Bluetooth: Stopping app", metadata: ["app": "\(appName)"])

        var response = Wendy_Agent_Services_V1_AppsStopResponse()

        do {
            try await Containerd.withClient { client in
                try await client.stopTask(containerID: appName)
            }
            await ContainerMonitor.shared.markContainerStopped(appName)
            response.success = true
        } catch {
            response.success = false
            response.errorMessage = error.localizedDescription
        }

        return response
    }

    private func handleAppsRemove(
        appName: String,
        purgeImage: Bool
    ) async throws -> Wendy_Agent_Services_V1_AppsRemoveResponse {
        logger.debug(
            "Bluetooth: Removing app",
            metadata: ["app": "\(appName)", "purgeImage": "\(purgeImage)"]
        )

        var response = Wendy_Agent_Services_V1_AppsRemoveResponse()

        do {
            try await Containerd.withClient { client in
                // Stop the task if running
                _ = try? await client.stopTask(containerID: appName)
                // Delete the container
                try await client.deleteContainer(named: appName)
                // Delete the image if requested
                if purgeImage {
                    try await client.deleteImage(named: appName)
                }
            }
            response.success = true
        } catch {
            response.success = false
            response.errorMessage = error.localizedDescription
        }

        return response
    }

    // MARK: - Hardware Handlers

    private func handleHardwareList() async throws
        -> Wendy_Agent_Services_V1_HardwareListResponse
    {
        logger.debug("Bluetooth: Listing hardware capabilities")

        let discoverer = SystemHardwareDiscoverer()
        let capabilities = try await discoverer.discoverCapabilities(categoryFilter: nil)

        var response = Wendy_Agent_Services_V1_HardwareListResponse()
        response.capabilities = capabilities.map { cap in
            var info = Wendy_Agent_Services_V1_HardwareInfo()
            info.type = cap.category
            info.name = cap.description
            info.available = true
            return info
        }

        return response
    }

    // MARK: - Helpers

    /// Gets the system hostname, using gethostname() on Linux for reliability
    private func getSystemHostname() -> String {
        #if os(Linux)
            var buffer = [CChar](repeating: 0, count: 256)
            if gethostname(&buffer, buffer.count) == 0 {
                // Find null terminator and decode as UTF-8
                let length = buffer.firstIndex(of: 0) ?? buffer.count
                return String(
                    decoding: buffer.prefix(length).map { UInt8(bitPattern: $0) },
                    as: UTF8.self
                )
            }
        #endif
        // Fallback to ProcessInfo
        let hostname = ProcessInfo.processInfo.hostName
        if !hostname.isEmpty && hostname != "localhost" {
            return hostname
        }
        return "WendyOS-Device"
    }

    /// Extracts a human-readable device name from a hostname
    /// For example: "wendyos-diligent-vessel" -> "Diligent Vessel"
    private func extractDeviceName(from hostname: String) -> String {
        var name = hostname

        // Remove common prefixes
        let prefixes = ["wendyos-", "wendy-"]
        for prefix in prefixes {
            if name.lowercased().hasPrefix(prefix) {
                name = String(name.dropFirst(prefix.count))
                break
            }
        }

        // Remove .local suffix if present
        if name.lowercased().hasSuffix(".local") {
            name = String(name.dropLast(6))
        }

        // Replace hyphens/underscores with spaces and capitalize words
        name =
            name
            .replacingOccurrences(of: "-", with: " ")
            .replacingOccurrences(of: "_", with: " ")
            .split(separator: " ")
            .map { $0.prefix(1).uppercased() + $0.dropFirst().lowercased() }
            .joined(separator: " ")

        // If name is empty after processing, use a fallback
        if name.isEmpty {
            return "WendyOS Device"
        }

        return name
    }
}

/// Errors that can occur in the Bluetooth service
enum BluetoothServiceError: Error, LocalizedError {
    case unauthorized
    case unsupported
    case poweredOff
    case connectionFailed
    case invalidConfiguration
    case stateStreamEnded
    case advertisingFailed(underlying: Error)
    case l2capPublishFailed(underlying: Error)
    case timeout(waitingFor: String, lastState: String)

    var errorDescription: String? {
        switch self {
        case .unauthorized:
            return "Bluetooth access is not authorized"
        case .unsupported:
            return "Bluetooth is not supported on this device"
        case .poweredOff:
            return "Bluetooth is powered off"
        case .connectionFailed:
            return "Failed to establish Bluetooth connection"
        case .invalidConfiguration:
            return "Invalid Bluetooth configuration"
        case .stateStreamEnded:
            return "Bluetooth state stream ended without reaching poweredOn state"
        case .advertisingFailed(let underlying):
            return "Failed to start Bluetooth advertising: \(underlying)"
        case .l2capPublishFailed(let underlying):
            return "Failed to publish L2CAP channel: \(underlying)"
        case .timeout(let waitingFor, let lastState):
            return "Timeout waiting for '\(waitingFor)' state (last state was '\(lastState)')"
        }
    }
}
