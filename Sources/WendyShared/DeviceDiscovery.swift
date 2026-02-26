import Bluetooth
import Logging
import NIOCore
import NIOFoundationCompat
import WendyAgentGRPC

#if canImport(FoundationEssentials)
    import FoundationEssentials
#else
    import Foundation
#endif

public protocol DeviceDiscovery: Sendable {
    func findUSBDevices() async -> [USBDevice]
    func findEthernetInterfaces() async -> [EthernetInterface]
    func findLANDevices() async throws -> [LANDevice]
    func findBluetoothDevices(resolveAgentVersion: Bool) async throws -> [BluetoothDevice]

    /// Discover LAN devices reachable via Wendy USB network interfaces.
    /// Uses IPv6 NDP to find peer link-local addresses on USB-connected devices.
    func findUSBLANDevices() async -> [LANDevice]

    /// Discover LAN devices and call the handler for each one as it's found
    func withLANDeviceDiscovery(_ handler: (LANDevice) async throws -> Void) async throws
}

extension DeviceDiscovery {
    /// Convenience method that calls findBluetoothDevices with resolveAgentVersion: false
    public func findBluetoothDevices() async throws -> [BluetoothDevice] {
        try await findBluetoothDevices(resolveAgentVersion: false)
    }

    /// Default implementation returns empty (platforms without USB gadget networking)
    public func findUSBLANDevices() async -> [LANDevice] {
        return []
    }

    /// Default implementation that wraps findLANDevices
    public func withLANDeviceDiscovery(_ handler: (LANDevice) async throws -> Void) async throws {
        let devices = try await findLANDevices()
        for device in devices {
            try await handler(device)
        }
    }
}

extension DeviceDiscovery {
    public func findAllDevices() async throws -> DevicesCollection {
        try await findDevices(includeBluetooth: true)
    }

    /// Find devices with optional Bluetooth scanning.
    /// BLE scan takes 5+ seconds, so skip when not needed (e.g., `wendy run`).
    public func findDevices(includeBluetooth: Bool) async throws -> DevicesCollection {
        if includeBluetooth {
            async let usbDevices = findUSBDevices()
            async let ethernetDevices = findEthernetInterfaces()
            async let lanDevices = findLANDevices()
            async let usbLANDevices = findUSBLANDevices()
            async let bluetoothDevices = findBluetoothDevices()

            let lan = try await lanDevices + usbLANDevices
            return try await DevicesCollection(
                usb: usbDevices,
                ethernet: ethernetDevices,
                lan: lan,
                bluetooth: bluetoothDevices
            )
        } else {
            async let usbDevices = findUSBDevices()
            async let ethernetDevices = findEthernetInterfaces()
            async let lanDevices = findLANDevices()
            async let usbLANDevices = findUSBLANDevices()

            let lan = try await lanDevices + usbLANDevices
            return try await DevicesCollection(
                usb: usbDevices,
                ethernet: ethernetDevices,
                lan: lan,
                bluetooth: []
            )
        }
    }
}

extension DeviceDiscovery {
    public func findBluetoothDevices(
        resolveAgentVersion: Bool = false
    ) async throws -> [BluetoothDevice] {
        let logger = Logger(label: "sh.wendy.bluetooth.discovery")
        logger.debug("Starting Bluetooth device discovery...")

        let centralManager = CentralManager()
        try await centralManager.waitUntilReady()

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

        let devices = Array(discoveredDevices.values).map(\.0).sorted { $0.rssi > $1.rssi }
        logger.debug("Bluetooth scan complete", metadata: ["deviceCount": "\(devices.count)"])

        return devices
    }

    private func resolveBluetoothAgentVersion(
        peripheral: Peripheral,
        centralManager: CentralManager
    ) async throws -> String {
        let logger = Logger(label: "sh.wendy.bluetooth.version-resolution")
        // Connect to peripheral
        let connection = try await centralManager.connect(to: peripheral)

        // Wait for connection
        do {
            try await withTimeout(of: .seconds(5)) {
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
        let commandData = try command.serializedData()

        // Send with length prefix using ByteBuffer
        var sendBuffer = ByteBuffer()
        try sendBuffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { buffer in
            buffer.writeData(commandData)
        }
        try await channel.send(Data(buffer: sendBuffer))

        // Read response using ByteBuffer
        var receiveBuffer = ByteBuffer()

        for try await data in channel.incoming() {
            receiveBuffer.writeData(data)

            // Try to read length prefix if we have enough bytes
            if receiveBuffer.readableBytes >= 2 {
                guard
                    let response = receiveBuffer.readLengthPrefixedSlice(
                        endianness: .big,
                        as: UInt16.self
                    )
                else {
                    continue
                }

                let bluetoothRespone = try Wendy_Agent_Services_V1_BluetoothResponse(
                    serializedBytes: Array(response.readableBytesView)
                )

                if case .agentVersion(let agentVersion) = bluetoothRespone.response {
                    return agentVersion.version
                }
                throw BluetoothVersionResolutionError.unexpectedResponse
            }
        }

        throw BluetoothVersionResolutionError.connectionClosed
    }

    private func withTimeout<T: Sendable>(
        of duration: Duration,
        operation: @Sendable @escaping () async throws -> T
    ) async throws -> T {
        try await withThrowingTaskGroup(of: T.self) { group in
            group.addTask {
                try await operation()
            }
            group.addTask {
                try await Task.sleep(for: duration)
                throw CancellationError()
            }
            let result = try await group.next()!
            group.cancelAll()
            return result
        }
    }
}

private enum BluetoothVersionResolutionError: Error {
    case connectionFailed
    case unexpectedResponse
    case connectionClosed
    case messageTooLarge
}

extension CentralManager {
    public func waitUntilReady(timeout: Duration = .seconds(5)) async throws {
        let logger = Logger(label: "sh.wendy.bluetooth.centralmanager")
        // Wait for Bluetooth to be ready
        let startTime = ContinuousClock.now

        var state = self.state()
        while state != .poweredOn {
            if ContinuousClock.now - startTime > timeout {
                logger.warning(
                    "Timeout waiting for Bluetooth to be ready",
                    metadata: ["lastState": "\(state)"]
                )
                throw CancellationError()
            }

            if state == .poweredOff || state == .unauthorized || state == .unsupported {
                logger.debug(
                    "Bluetooth not available",
                    metadata: ["state": "\(state)"]
                )
                throw CancellationError()
            }

            // Wait for state updates
            for await newState in stateUpdates() {
                state = newState
                if state == .poweredOn {
                    break
                }
                if state == .poweredOff || state == .unauthorized || state == .unsupported {
                    logger.debug(
                        "Bluetooth not available",
                        metadata: ["state": "\(state)"]
                    )
                    throw CancellationError()
                }
                if ContinuousClock.now - startTime > timeout {
                    break
                }
            }
        }
    }
}
