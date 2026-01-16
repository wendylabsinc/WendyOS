import Bluetooth
import ContainerdGRPC
import Foundation
import Logging
import ServiceLifecycle
import WendyShared

#if canImport(Glibc)
import Glibc
#elseif canImport(Musl)
import Musl
#endif

/// Service that exposes the WendyOS agent over Bluetooth Low Energy
/// This allows CLI clients to discover and communicate with devices without network connectivity
actor BluetoothService: Service {
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
                logger.warning("Advertising failed, but continuing with L2CAP listener", metadata: [
                    "error": "\(error)",
                    "hint": "Device may not be discoverable, but direct connections may still work"
                ])
            }

            // Handle incoming L2CAP connections
            currentPhase = .handlingConnections
            logger.debug("Phase: handling incoming L2CAP connections")
            try await handleConnections(manager)
        } catch is CancellationError {
            logger.debug("Bluetooth service cancelled", metadata: ["phase": "\(currentPhase.rawValue)"])
        } catch {
            // Log the error but don't crash the entire agent
            // Bluetooth is optional functionality
            logger.warning("Bluetooth service failed (agent continues without Bluetooth)", metadata: [
                "error": "\(error)",
                "errorType": "\(type(of: error))",
                "errorDescription": "\((error as? LocalizedError)?.errorDescription ?? "N/A")",
                "phase": "\(currentPhase.rawValue)"
            ])

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
        logger.debug("Waiting for Bluetooth to be ready...")

        // Check current state first (don't wait for updates if already ready)
        let currentState = await manager.state()
        logger.trace("Current Bluetooth state", metadata: ["state": "\(currentState)"])

        switch currentState {
        case .poweredOn:
            logger.debug("Bluetooth is already powered on and ready")
            return
        case .poweredOff:
            logger.error("Bluetooth is powered off")
            throw BluetoothServiceError.poweredOff
        case .unauthorized:
            logger.error("Bluetooth access is unauthorized")
            throw BluetoothServiceError.unauthorized
        case .unsupported:
            logger.error("Bluetooth is not supported on this device")
            throw BluetoothServiceError.unsupported
        case .unknown:
            // On Linux/BlueZ, the state might be reported as unknown even when the adapter is powered on.
            // The BlueZ library may not correctly read the Powered property.
            // We'll proceed optimistically and let actual Bluetooth operations fail if there's a real issue.
            logger.warning("Bluetooth state is unknown - proceeding optimistically (BlueZ may not report state correctly)")
            return
        case .resetting:
            // State is resetting, wait for it to stabilize
            logger.debug("Bluetooth is resetting, waiting for stabilization...")
        }

        // Wait for state updates with timeout (only reached if state is .resetting)
        logger.debug("Creating state updates stream...")
        let timeoutDuration: Duration = .seconds(30)
        let startTime = ContinuousClock.now
        var stateUpdateCount = 0
        var lastState: String = "\(currentState)"

        for await state in await manager.stateUpdates() {
            stateUpdateCount += 1
            lastState = "\(state)"
            let elapsed = ContinuousClock.now - startTime

            logger.debug("Received Bluetooth state update", metadata: [
                "state": "\(state)",
                "updateNumber": "\(stateUpdateCount)",
                "elapsedSeconds": "\(elapsed.components.seconds)"
            ])

            // Check for timeout
            if elapsed > timeoutDuration {
                logger.error("Timeout waiting for Bluetooth to be ready", metadata: [
                    "lastState": "\(lastState)",
                    "totalUpdates": "\(stateUpdateCount)",
                    "timeoutSeconds": "\(timeoutDuration.components.seconds)"
                ])
                throw BluetoothServiceError.timeout(waitingFor: "poweredOn", lastState: lastState)
            }

            switch state {
            case .poweredOn:
                logger.debug("Bluetooth is powered on and ready", metadata: [
                    "elapsedSeconds": "\(elapsed.components.seconds)"
                ])
                return
            case .poweredOff:
                logger.error("Bluetooth is powered off")
                throw BluetoothServiceError.poweredOff
            case .unauthorized:
                logger.error("Bluetooth access is unauthorized")
                throw BluetoothServiceError.unauthorized
            case .unsupported:
                logger.error("Bluetooth is not supported on this device")
                throw BluetoothServiceError.unsupported
            case .resetting:
                logger.debug("Bluetooth is resetting...")
            case .unknown:
                // On Linux/BlueZ, proceed if we get unknown state
                logger.warning("Bluetooth state changed to unknown - proceeding optimistically")
                return
            }
        }

        // If we get here, the stream ended without reaching poweredOn
        // On Linux/BlueZ this might happen, proceed optimistically
        logger.warning("Bluetooth state stream ended without definitive state - proceeding optimistically", metadata: [
            "totalUpdates": "\(stateUpdateCount)",
            "lastState": "\(lastState)"
        ])
    }

    private func startAdvertising(_ manager: PeripheralManager) async throws {
        // Get hostname for device name - use gethostname() on Linux for reliability
        let hostname = getSystemHostname()
        let deviceName = extractDeviceName(from: hostname)
        logger.info("Preparing advertisement", metadata: [
            "hostname": "\(hostname)",
            "advertisingName": "\(deviceName)"
        ])

        // Convert string UUID to Foundation UUID
        guard let serviceUUID = UUID(uuidString: WendyBluetoothUUIDs.serviceUUID) else {
            logger.error("Invalid service UUID configuration", metadata: [
                "serviceUUID": "\(WendyBluetoothUUIDs.serviceUUID)"
            ])
            throw BluetoothServiceError.invalidConfiguration
        }

        // deviceName was already extracted above for logging
        let shortName = deviceName

        // Calculate approximate advertising data size:
        // - Flags: 3 bytes
        // - Complete Local Name: 2 + name.utf8.count bytes
        // Total must be <= 31 bytes for legacy advertising
        // If name is too long, truncate it
        let maxNameLength = 31 - 3 - 2  // 26 bytes for name
        let advertisingName: String
        if shortName.utf8.count > maxNameLength {
            // Truncate to fit, ensuring we don't cut in the middle of a multi-byte character
            var truncated = shortName
            while truncated.utf8.count > maxNameLength && !truncated.isEmpty {
                truncated.removeLast()
            }
            advertisingName = truncated
            logger.debug("Truncated advertising name to fit legacy limit", metadata: [
                "original": "\(shortName)",
                "truncated": "\(advertisingName)",
                "originalBytes": "\(shortName.utf8.count)",
                "truncatedBytes": "\(advertisingName.utf8.count)"
            ])
        } else {
            advertisingName = shortName
        }

        let advertisementData = AdvertisementData(
            localName: advertisingName,
            serviceUUIDs: []  // Omit UUID to stay under legacy 31-byte limit
        )

        logger.debug("Starting Bluetooth advertising", metadata: [
            "advertisingName": "\(advertisingName)",
            "nameBytes": "\(advertisingName.utf8.count)",
            "serviceUUID": "\(serviceUUID) (not included in advertising)"
        ])

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
            logger.error("Failed to start advertising", metadata: [
                "error": "\(error)",
                "errorType": "\(type(of: error))"
            ])
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
            logger.debug("L2CAP channel published successfully", metadata: ["psm": "\(psm.rawValue)"])
        } catch {
            logger.error("Failed to publish L2CAP channel", metadata: [
                "error": "\(error)",
                "errorType": "\(type(of: error))"
            ])
            throw BluetoothServiceError.l2capPublishFailed(underlying: error)
        }

        // Skip PSM update for now - BlueZ has issues with stop/restart advertising
        // The CLI will use the default PSM (128)
        logger.debug("Skipping PSM service data update (BlueZ compatibility)", metadata: ["psm": "\(psm.rawValue)"])

        // Get the incoming channel stream
        logger.debug("Creating incoming L2CAP channel stream...")
        let incomingChannels: AsyncThrowingStream<any L2CAPChannel, Error>
        do {
            incomingChannels = try await manager.incomingL2CAPChannels(psm: psm)
            logger.debug("Incoming L2CAP channel stream created, waiting for connections...", metadata: ["psm": "\(psm.rawValue)"])
        } catch {
            logger.error("Failed to get incoming L2CAP channels", metadata: [
                "error": "\(error)",
                "errorType": "\(type(of: error))"
            ])
            throw error
        }

        try await withGracefulShutdownHandler {
            self.logger.debug("Entering connection accept loop...")
            var connectionCount = 0

            try await withThrowingDiscardingTaskGroup { group in
                for try await channel in incomingChannels {
                    connectionCount += 1
                    self.logger.debug("New Bluetooth connection established", metadata: [
                        "connectionNumber": "\(connectionCount)"
                    ])

                    // Handle each connection in a child task
                    group.addTask {
                        await self.handleChannel(channel)
                    }
                }
            }

            self.logger.debug("L2CAP channel stream ended", metadata: [
                "totalConnections": "\(connectionCount)"
            ])
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

        do {
            for try await data in channel.incoming() {
                messagesReceived += 1
                logger.debug("Received Bluetooth message", metadata: [
                    "messageNumber": "\(messagesReceived)",
                    "dataSize": "\(data.count)"
                ])
                let response = await processCommand(data)
                logger.debug("Sending response", metadata: ["responseSize": "\(response.count)"])
                try await channel.send(response)
            }
        } catch {
            // Socket closed after processing messages is expected (client disconnected)
            let errorString = String(describing: error)
            if messagesReceived > 0 && errorString.contains("socket closed") {
                logger.debug("Client disconnected after request/response", metadata: [
                    "messagesProcessed": "\(messagesReceived)"
                ])
            } else {
                logger.error("Error handling Bluetooth channel", metadata: [
                    "error": "\(error)",
                    "errorType": "\(type(of: error))",
                    "messagesProcessed": "\(messagesReceived)"
                ])
            }
        }

        logger.debug("Bluetooth connection closed", metadata: [
            "totalMessagesProcessed": "\(messagesReceived)"
        ])
    }

    private func processCommand(_ data: Data) async -> Data {
        do {
            let command = try BluetoothAgentCommand.from(data: data)
            let response = try await executeCommand(command)
            return try response.toData()
        } catch {
            logger.error("Error processing Bluetooth command", metadata: ["error": "\(error)"])
            let errorResponse = BluetoothResponse.error(message: error.localizedDescription)
            return (try? errorResponse.toData()) ?? Data()
        }
    }

    private func executeCommand(_ command: BluetoothAgentCommand) async throws -> BluetoothResponse
    {
        switch command {
        case .wifiList:
            return try await handleWifiList()
        case .wifiConnect(let ssid, let password):
            return try await handleWifiConnect(ssid: ssid, password: password)
        case .wifiStatus:
            return try await handleWifiStatus()
        case .wifiDisconnect:
            return try await handleWifiDisconnect()
        case .appsList:
            return try await handleAppsList()
        case .appsStop(let appName):
            return try await handleAppsStop(appName: appName)
        case .appsRemove(let appName, let purgeImage):
            return try await handleAppsRemove(appName: appName, purgeImage: purgeImage)
        case .agentVersion:
            return .agentVersion(version: Version.current)
        case .hardwareList:
            return try await handleHardwareList()
        }
    }

    // MARK: - WiFi Handlers

    private func handleWifiList() async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Listing WiFi networks")

        let networkManager = try await networkManagerFactory.createNetworkManager(
            preference: configuration.networkManagerPreference
        )
        let networks = try await networkManager.listWiFiNetworks()

        let networkInfos = networks.map { network in
            WiFiNetworkInfo(
                ssid: network.ssid,
                signalStrength: network.signalStrength.map { Int($0) }
            )
        }

        return .wifiList(networks: networkInfos)
    }

    private func handleWifiConnect(ssid: String, password: String) async throws -> BluetoothResponse
    {
        logger.debug("Bluetooth: Connecting to WiFi", metadata: ["ssid": "\(ssid)"])

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            try await networkManager.setupWiFi(ssid: ssid, password: password)

            if let connection = try await networkManager.getCurrentConnection() {
                logger.debug("Connected to WiFi", metadata: ["ssid": "\(connection.ssid)"])
                return .wifiConnect(success: true, errorMessage: nil)
            } else {
                return .wifiConnect(success: false, errorMessage: "Connection failed")
            }
        } catch {
            return .wifiConnect(success: false, errorMessage: error.localizedDescription)
        }
    }

    private func handleWifiStatus() async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Getting WiFi status")

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            let connection = try await networkManager.getCurrentConnection()

            if let connection {
                return .wifiStatus(connected: true, ssid: connection.ssid, errorMessage: nil)
            } else {
                return .wifiStatus(connected: false, ssid: nil, errorMessage: nil)
            }
        } catch {
            return .wifiStatus(
                connected: false,
                ssid: nil,
                errorMessage: error.localizedDescription
            )
        }
    }

    private func handleWifiDisconnect() async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Disconnecting from WiFi")

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )

            if (try await networkManager.getCurrentConnection()) != nil {
                let success = try await networkManager.disconnectFromNetwork()
                return .wifiDisconnect(success: success, errorMessage: nil)
            } else {
                return .wifiDisconnect(success: true, errorMessage: nil)
            }
        } catch NetworkConnectionError.noActiveConnection {
            return .wifiDisconnect(success: true, errorMessage: nil)
        } catch {
            return .wifiDisconnect(success: false, errorMessage: error.localizedDescription)
        }
    }

    // MARK: - Apps Handlers

    private func handleAppsList() async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Listing apps")

        let (containers, tasks) = try await Containerd.withClient { client in
            let tasks = try await client.listTasks()
            let containers = try await client.listContainers()
            return (containers, tasks)
        }

        var apps: [AppInfo] = []

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
                Int(container.labels["sh.wendy/failure.count"] ?? "0") ?? 0

            apps.append(
                AppInfo(
                    appName: container.id,
                    appVersion: version,
                    state: state,
                    failureCount: failureCount
                )
            )
        }

        return .appsList(apps: apps)
    }

    private func handleAppsStop(appName: String) async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Stopping app", metadata: ["app": "\(appName)"])

        do {
            try await Containerd.withClient { client in
                try await client.stopTask(containerID: appName)
            }
            await ContainerMonitor.shared.markContainerStopped(appName)
            return .appsStop(success: true, errorMessage: nil)
        } catch {
            return .appsStop(success: false, errorMessage: error.localizedDescription)
        }
    }

    private func handleAppsRemove(
        appName: String,
        purgeImage: Bool
    ) async throws
        -> BluetoothResponse
    {
        logger.debug(
            "Bluetooth: Removing app",
            metadata: ["app": "\(appName)", "purgeImage": "\(purgeImage)"]
        )

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
            return .appsRemove(success: true, errorMessage: nil)
        } catch {
            return .appsRemove(success: false, errorMessage: error.localizedDescription)
        }
    }

    // MARK: - Hardware Handlers

    private func handleHardwareList() async throws -> BluetoothResponse {
        logger.debug("Bluetooth: Listing hardware capabilities")

        let discoverer = SystemHardwareDiscoverer()
        let capabilities = try await discoverer.discoverCapabilities(categoryFilter: nil)

        let capabilityInfos = capabilities.map { cap in
            WendyShared.BluetoothHardwareInfo(
                type: cap.category,
                name: cap.description,
                available: true
            )
        }

        return .hardwareList(capabilities: capabilityInfos)
    }

    // MARK: - Helpers

    /// Gets the system hostname, using gethostname() on Linux for reliability
    private func getSystemHostname() -> String {
        #if os(Linux)
        var buffer = [CChar](repeating: 0, count: 256)
        if gethostname(&buffer, buffer.count) == 0 {
            return String(cString: buffer)
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
        name = name
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
