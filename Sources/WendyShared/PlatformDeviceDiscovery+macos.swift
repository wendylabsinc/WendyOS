#if os(macOS)
    import AsyncDNSResolver
    import Bluetooth
    import Foundation
    import Logging
    import IOKit
    import IOKit.usb
    import Network
    import SystemConfiguration

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
            var interfaces: [LANDevice] = []

            let resolver = try AsyncDNSResolver()
            let ptrWendy = try await resolver.queryPTR(name: "_wendyos._udp.local")
            let ptrEdge = try await resolver.queryPTR(name: "_edgeos._udp.local")
            for name in (ptrWendy.names + ptrEdge.names) {
                guard let srv = try await resolver.querySRV(name: name).first else {
                    continue
                }

                let txt = try? await resolver.queryTXT(name: name).first
                let id = txt?.txt.split(separator: "=").last.map(String.init) ?? ""

                let lanDevice = LANDevice(
                    id: id,
                    displayName: id,
                    hostname: srv.host,
                    port: Int(srv.port),
                    interfaceType: "LAN",
                    isWendyDevice: true
                )

                // Prevent duplicates
                if !interfaces.contains(where: { $0.id == id || $0.hostname == srv.host }) {
                    interfaces.append(lanDevice)
                }
            }

            return interfaces
        }

        public func findBluetoothDevices() async throws -> [BluetoothDevice] {
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

            var discoveredDevices: [String: BluetoothDevice] = [:]
            let scanDuration: Duration = .seconds(5)
            let scanStartTime = ContinuousClock.now

            do {
                // Start scanning for devices - no filter to discover all WendyOS devices
                // The service UUID may not be in advertising data due to 31-byte limit
                let discoveries = try await centralManager.scan()

                for try await discovery in discoveries {
                    let elapsed = ContinuousClock.now - scanStartTime
                    if elapsed > scanDuration {
                        break
                    }

                    // Check if this is a Wendy device by looking at the local name
                    let localName =
                        discovery.advertisementData.localName ?? discovery.peripheral.name ?? ""
                    let isWendyDevice = localName.lowercased().contains("wendy")

                    if isWendyDevice {
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
                            agentVersion: nil,  // Will be resolved separately
                            l2capPSM: WendyBluetoothUUIDs.l2capPSM
                        )

                        // Only add if not already discovered (keep the one with better RSSI)
                        if let existing = discoveredDevices[deviceId] {
                            if rssi > existing.rssi {
                                discoveredDevices[deviceId] = device
                            }
                        } else {
                            discoveredDevices[deviceId] = device
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
            } catch {
                logger.warning(
                    "Bluetooth scan failed",
                    metadata: ["error": "\(error)"]
                )
                return []
            }

            let devices = Array(discoveredDevices.values).sorted { $0.rssi > $1.rssi }
            logger.debug("Bluetooth scan complete", metadata: ["deviceCount": "\(devices.count)"])

            return devices
        }
    }
#endif
