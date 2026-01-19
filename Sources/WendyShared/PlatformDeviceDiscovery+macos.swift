#if os(macOS)
    import AsyncDNSResolver
    import Bluetooth
    import Foundation
    import Logging
    import IOKit
    import IOKit.usb
    import Network
    import NIOCore
    import NIOFoundationCompat
    import SystemConfiguration
    import WendyAgentGRPC

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let ioServiceProvider: IOServiceProvider
        private let networkInterfaceProvider: NetworkInterfaceProvider
        private let logger: Logger

        public init(
            ioServiceProvider: IOServiceProvider = DefaultIOServiceProvider(),
            networkInterfaceProvider: NetworkInterfaceProvider = DefaultNetworkInterfaceProvider(),
            logger: Logger
        ) {
            self.ioServiceProvider = ioServiceProvider
            self.networkInterfaceProvider = networkInterfaceProvider
            self.logger = logger
        }

        public func findUSBDevices() async -> [USBDevice] {
            var devices: [USBDevice] = []
            let matchingDict = ioServiceProvider.createMatchingDictionary(
                className: kIOUSBDeviceClassName
            )
            var iterator: io_iterator_t = 0
            defer { ioServiceProvider.releaseIOObject(object: iterator) }

            let result = ioServiceProvider.getMatchingServices(
                masterPort: kIOMainPortDefault,
                matchingDict: matchingDict,
                iterator: &iterator
            )

            if result != KERN_SUCCESS {
                logger.error(
                    "Error: Failed to get matching services",
                    metadata: ["result": .string(String(result))]
                )
                return devices
            }

            var usbDevice = ioServiceProvider.getNextItem(iterator: iterator)

            while usbDevice != 0 {
                if let device = USBDevice.fromIORegistryEntry(
                    usbDevice,
                    provider: ioServiceProvider
                ) {
                    logger.debug(
                        "Found device",
                        metadata: ["device": .string(device.toHumanReadableString())]
                    )
                    // Only track Wendy devices
                    if device.isWendyDevice {
                        devices.append(device)
                        logger.debug(
                            "Wendy device found",
                            metadata: ["deviceName": .string(device.name)]
                        )
                    }
                }

                ioServiceProvider.releaseIOObject(object: usbDevice)
                usbDevice = ioServiceProvider.getNextItem(iterator: iterator)
            }

            if devices.isEmpty {
                logger.debug("No Wendy devices found.")
            }

            return devices
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
            var interfaces: [EthernetInterface] = []

            guard let scInterfaces = networkInterfaceProvider.copyAllNetworkInterfaces() else {
                logger.error("Failed to get network interfaces")
                return interfaces
            }

            let linkSpeeds = networkInterfaceProvider.getAllLinkSpeeds()

            for interface in scInterfaces {
                // Check if it's an Ethernet interface
                guard
                    let interfaceType = networkInterfaceProvider.getInterfaceType(
                        interface: interface
                    ),
                    interfaceType == kSCNetworkInterfaceTypeEthernet as String
                        || interfaceType == kSCNetworkInterfaceTypeIEEE80211 as String  // WiFi
                        || interfaceType == kSCNetworkInterfaceTypePPP as String
                        || interfaceType == kSCNetworkInterfaceTypeBond as String
                else {
                    continue
                }

                // Get interface details
                let name = networkInterfaceProvider.getBSDName(interface: interface) ?? "Unknown"
                let displayName =
                    networkInterfaceProvider.getLocalizedDisplayName(interface: interface)
                    ?? "Unknown"

                // Only collect interfaces containing "Wendy" in their name
                if !displayName.contains("Wendy") && !name.contains("Wendy") {
                    continue
                }

                // Get MAC address for physical interfaces
                var macAddress: String? = nil
                if interfaceType == kSCNetworkInterfaceTypeEthernet as String
                    || interfaceType == kSCNetworkInterfaceTypeIEEE80211 as String
                {
                    macAddress = networkInterfaceProvider.getHardwareAddressString(
                        interface: interface
                    )
                }

                // Look up link speed from pre-fetched data
                let linkSpeed = linkSpeeds[name]

                let ethernetInterface = EthernetInterface(
                    name: name,
                    displayName: displayName,
                    interfaceType: interfaceType,
                    macAddress: macAddress,
                    linkSpeed: linkSpeed
                )

                interfaces.append(ethernetInterface)
                logger.debug("Wendy interface found", metadata: ["interface": .string(displayName)])
            }

            if interfaces.isEmpty {
                logger.debug("No Wendy Ethernet interfaces found.")
            }

            return interfaces
        }

        public func findLANDevices() async throws -> [LANDevice] {
            var devices: [LANDevice] = []
            try await withLANDeviceDiscovery { device in
                devices.append(device)
            }
            return devices
        }

        public func withLANDeviceDiscovery(_ handler: (LANDevice) async throws -> Void) async throws {
            var seenDevices: Set<String> = []

            // Run PTR query with 3-second timeout
            let allNames: [String]
            do {
                allNames = try await withThrowingTaskGroup(of: [String].self) { group in
                    group.addTask {
                        let resolver = try AsyncDNSResolver()
                        return try await resolver.queryPTR(name: "_wendyos._udp.local").names
                    }
                    group.addTask {
                        try await Task.sleep(for: .seconds(3))
                        throw CancellationError()
                    }
                    let result = try await group.next() ?? []
                    group.cancelAll()
                    return result
                }
            } catch is CancellationError {
                return
            }

            let resolver = try AsyncDNSResolver()

            for name in allNames {
                try Task.checkCancellation()

                guard let srv = try? await resolver.querySRV(name: name).first else {
                    continue
                }

                let txt = try? await resolver.queryTXT(name: name).first
                let id = txt?.txt.split(separator: "=").last.map(String.init) ?? ""

                let key = "\(id)-\(srv.host)"
                guard !seenDevices.contains(key) else { continue }
                seenDevices.insert(key)

                let lanDevice = LANDevice(
                    id: id,
                    displayName: id,
                    hostname: srv.host,
                    port: Int(srv.port),
                    interfaceType: "LAN",
                    isWendyDevice: true
                )

                try await handler(lanDevice)
            }
        }

        public func findBluetoothDevices(
            resolveAgentVersion: Bool = false
        ) async throws -> [BluetoothDevice] {
            logger.debug("Starting Bluetooth device discovery...")

            let centralManager = CentralManager()

            // Wait for Bluetooth to be ready
            let startTime = ContinuousClock.now
            let timeout: Duration = .seconds(5)

            var state = await centralManager.state()
            while state != .poweredOn {
                if ContinuousClock.now - startTime > timeout {
                    logger.warning(
                        "Timeout waiting for Bluetooth to be ready",
                        metadata: ["lastState": "\(state)"]
                    )
                    return []
                }

                if state == .poweredOff || state == .unauthorized || state == .unsupported {
                    logger.warning(
                        "Bluetooth not available",
                        metadata: ["state": "\(state)"]
                    )
                    return []
                }

                // Wait for state updates
                for await newState in await centralManager.stateUpdates() {
                    state = newState
                    if state == .poweredOn {
                        break
                    }
                    if state == .poweredOff || state == .unauthorized || state == .unsupported {
                        logger.warning(
                            "Bluetooth not available",
                            metadata: ["state": "\(state)"]
                        )
                        return []
                    }
                    if ContinuousClock.now - startTime > timeout {
                        break
                    }
                }
            }

            logger.debug("Bluetooth is ready, starting scan...")

            var discoveredDevices: [String: (BluetoothDevice, Peripheral)] = [:]
            let scanDuration: Duration = .seconds(5)
            let scanStartTime = ContinuousClock.now

            // Create BluetoothUUID for filtering
            guard let foundationUUID = UUID(uuidString: WendyBluetoothUUIDs.serviceUUID) else {
                logger.warning("Invalid Wendy service UUID configuration")
                return []
            }
            let wendyServiceUUID = BluetoothUUID.bit128(foundationUUID)

            do {
                // Start scanning for devices advertising the Wendy service UUID
                let scanFilter = ScanFilter(serviceUUIDs: [wendyServiceUUID])
                let discoveries = try await centralManager.scan(filter: scanFilter)

                for try await discovery in discoveries {
                    let elapsed = ContinuousClock.now - scanStartTime
                    if elapsed > scanDuration {
                        break
                    }

                    // Check if this is a Wendy device by checking for our service UUID
                    let advertisedServiceUUIDs = discovery.advertisementData.serviceUUIDs
                    let isWendyDevice = advertisedServiceUUIDs.contains(wendyServiceUUID)

                    if isWendyDevice {
                        let localName =
                            discovery.advertisementData.localName ?? discovery.peripheral.name ?? ""
                        let deviceId = discovery.peripheral.id.rawValue
                        let displayName =
                            localName.isEmpty ? "WendyOS Device" : localName

                        let rssi = discovery.rssi

                        // Get the Bluetooth address (on macOS, we use the peripheral identifier)
                        let address = deviceId

                        let device = BluetoothDevice(
                            id: deviceId,
                            displayName: displayName,
                            address: address,
                            rssi: rssi,
                            isWendyDevice: true,
                            agentVersion: nil,
                            l2capPSM: WendyBluetoothUUIDs.l2capPSM
                        )

                        // Store peripheral for version resolution
                        let peripheral = discovery.peripheral

                        // Only add if not already discovered (keep the one with better RSSI)
                        if let existing = discoveredDevices[deviceId] {
                            if rssi > existing.0.rssi {
                                discoveredDevices[deviceId] = (device, peripheral)
                            }
                        } else {
                            discoveredDevices[deviceId] = (device, peripheral)
                            logger.debug(
                                "Discovered Wendy Bluetooth device",
                                metadata: [
                                    "name": "\(displayName)",
                                    "rssi": "\(rssi)",
                                ]
                            )
                        }
                    }
                }

                try await centralManager.stopScan()

                // Resolve agent versions if requested
                if resolveAgentVersion {
                    await withTaskGroup(of: (String, String?).self) { group in
                        for (deviceId, (_, peripheral)) in discoveredDevices {
                            group.addTask {
                                do {
                                    let version = try await self.resolveBluetoothAgentVersion(
                                        peripheral: peripheral,
                                        centralManager: centralManager
                                    )
                                    return (deviceId, version)
                                } catch {
                                    return (deviceId, nil)
                                }
                            }
                        }

                        for await (deviceId, version) in group {
                            if let version,
                                var entry = discoveredDevices[deviceId]
                            {
                                entry.0.agentVersion = version
                                discoveredDevices[deviceId] = entry
                            }
                        }
                    }
                }
            } catch {
                logger.warning(
                    "Bluetooth scan failed",
                    metadata: ["error": "\(error)"]
                )
                return []
            }

            let devices = discoveredDevices.values.map(\.0).sorted { $0.rssi > $1.rssi }
            logger.debug("Bluetooth scan complete", metadata: ["deviceCount": "\(devices.count)"])

            return devices
        }

        /// Resolve agent version for a Bluetooth device via L2CAP connection
        private func resolveBluetoothAgentVersion(
            peripheral: Peripheral,
            centralManager: CentralManager
        ) async throws -> String {
            logger.debug("Resolving agent version for \(peripheral.name ?? "unknown")")

            // Connect to the peripheral
            let connection = try await centralManager.connect(to: peripheral)

            // Wait for connection to be ready
            switch await connection.state() {
            case .connected:
                ()
            case .disconnected, .connecting:
                stateUpdates: for await newState in await connection.stateUpdates() {
                    switch newState {
                    case .connected:
                        break stateUpdates
                    case .disconnected:
                        throw BluetoothVersionResolutionError.connectionFailed
                    case .connecting:
                        continue stateUpdates
                    }
                }
            }

            // TODO: Replace with async defer in Swift 6.3
            defer {
                Task { [logger] in
                    logger.debug("Disconnecting from peripheral during cleanup")
                    await connection.disconnect()
                }
            }

            // Open L2CAP channel
            let psm = L2CAPPSM(rawValue: WendyBluetoothUUIDs.l2capPSM)
            let channel = try await connection.openL2CAPChannel(psm: psm)

            // TODO: Replace with async defer in Swift 6.3
            defer {
                Task { [logger] in
                    logger.debug("Closing L2CAP channel during cleanup")
                    await channel.close()
                }
            }

            // Send version request
            var command = Wendy_Agent_Services_V1_BluetoothCommand()
            command.agentVersion = Wendy_Agent_Services_V1_AgentVersionCommand()
            let commandBytes: [UInt8] = try command.serializedBytes()

            // Send with length prefix using ByteBuffer
            var sendBuffer = ByteBuffer()
            sendBuffer.writeInteger(UInt32(commandBytes.count), endianness: .big)
            sendBuffer.writeBytes(commandBytes)
            try await channel.send(Data(buffer: sendBuffer))

            // Read response using ByteBuffer
            var receiveBuffer = ByteBuffer()

            for try await data in channel.incoming() {
                receiveBuffer.writeData(data)

                // Try to read length prefix if we have enough bytes
                if receiveBuffer.readableBytes >= 4 {
                    let readerIndex = receiveBuffer.readerIndex
                    guard
                        let messageLength = receiveBuffer.readInteger(
                            endianness: .big,
                            as: UInt32.self
                        )
                    else {
                        // Reset and continue waiting for more data
                        receiveBuffer.moveReaderIndex(to: readerIndex)
                        continue
                    }

                    // Validate message size (cap to UInt16.max for BLE)
                    if messageLength > UInt32(UInt16.max) {
                        throw BluetoothVersionResolutionError.messageTooLarge
                    }

                    if receiveBuffer.readableBytes >= messageLength {
                        guard
                            let responseBytes = receiveBuffer.readBytes(length: Int(messageLength))
                        else {
                            throw BluetoothVersionResolutionError.unexpectedResponse
                        }
                        let response = try Wendy_Agent_Services_V1_BluetoothResponse(
                            serializedBytes: responseBytes
                        )

                        if case .agentVersion(let versionResponse) = response.response {
                            return versionResponse.version
                        }
                        throw BluetoothVersionResolutionError.unexpectedResponse
                    } else {
                        // Not enough data yet, reset reader index and wait for more
                        receiveBuffer.moveReaderIndex(to: readerIndex)
                    }
                }
            }

            throw BluetoothVersionResolutionError.connectionClosed
        }
    }

    private enum BluetoothVersionResolutionError: Error {
        case connectionFailed
        case unexpectedResponse
        case connectionClosed
        case messageTooLarge
    }
#endif
