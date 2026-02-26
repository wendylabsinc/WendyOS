#if os(Linux)
    import DNSClient
    import NIOCore
    import Foundation
    import Logging
    #if os(Linux)
        import Subprocess
    #endif

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let logger: Logger
        package var timeout: NIOCore.TimeAmount = .seconds(5)

        public init(
            logger: Logger
        ) {
            self.logger = logger
        }

        public func findUSBDevices() async -> [USBDevice] {
            #if os(Linux)
                logger.info("Listing USB devices on Linux")
                var devices: [USBDevice] = []

                do {
                    let sysPath = "/sys/bus/usb/devices"
                    let entries = try FileManager.default.contentsOfDirectory(atPath: sysPath)

                    for entry in entries {
                        let devicePath = "\(sysPath)/\(entry)"

                        // Read manufacturer and product strings from sysfs
                        let manufacturer = (try? String(
                            contentsOfFile: "\(devicePath)/manufacturer", encoding: .utf8
                        ))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                        let product = (try? String(
                            contentsOfFile: "\(devicePath)/product", encoding: .utf8
                        ))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""

                        // Check if this is a Wendy device by manufacturer or product string
                        let combined = "\(manufacturer) \(product)"
                        guard combined.localizedCaseInsensitiveContains("Wendy")
                            || combined.localizedCaseInsensitiveContains("WendyOS")
                        else {
                            continue
                        }

                        // Read vendor and product IDs
                        guard
                            let vidStr = try? String(
                                contentsOfFile: "\(devicePath)/idVendor", encoding: .utf8
                            ).trimmingCharacters(in: .whitespacesAndNewlines),
                            let pidStr = try? String(
                                contentsOfFile: "\(devicePath)/idProduct", encoding: .utf8
                            ).trimmingCharacters(in: .whitespacesAndNewlines),
                            let vendorId = Int(vidStr, radix: 16),
                            let productId = Int(pidStr, radix: 16)
                        else {
                            continue
                        }

                        // Read optional fields
                        let serialNumber = (try? String(
                            contentsOfFile: "\(devicePath)/serial", encoding: .utf8
                        ))?.trimmingCharacters(in: .whitespacesAndNewlines)

                        let bcdUSB = (try? String(
                            contentsOfFile: "\(devicePath)/version", encoding: .utf8
                        ))?.trimmingCharacters(in: .whitespacesAndNewlines)

                        var usbVersion: String? = nil
                        if let bcd = bcdUSB {
                            let trimmed = bcd.trimmingCharacters(in: .whitespaces)
                            if trimmed.hasPrefix("3.") {
                                usbVersion = "USB 3"
                            } else if trimmed.hasPrefix("2.") {
                                usbVersion = "USB 2"
                            }
                        }

                        let name = product.isEmpty ? manufacturer : product

                        devices.append(
                            USBDevice(
                                name: name,
                                vendorId: vendorId,
                                productId: productId,
                                usbVersion: usbVersion,
                                serialNumber: serialNumber
                            )
                        )

                        logger.info(
                            "Found Wendy USB device: \(name)",
                            metadata: [
                                "vendorId": .string(String(format: "0x%04X", vendorId)),
                                "productId": .string(String(format: "0x%04X", productId)),
                                "manufacturer": .string(manufacturer),
                                "sysfs": .string(entry),
                            ]
                        )
                    }
                } catch {
                    logger.error("Failed to list USB devices: \(error)")
                }

                if devices.isEmpty {
                    logger.info("No Wendy USB devices found.")
                }

                return devices
            #else
                logger.warning("USB device listing is not yet supported on Windows")
                return []
            #endif
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
            #if os(Linux)
                logger.info("Listing Ethernet interfaces on Linux")
                var interfaces: [EthernetInterface] = []

                // First, find network interfaces belonging to Wendy USB devices via sysfs
                let wendyNetInterfaces = findWendyUSBNetInterfaces()

                do {
                    // Read interface list from /sys/class/net
                    let interfaceNames = try FileManager.default.contentsOfDirectory(
                        atPath: "/sys/class/net"
                    )

                    for interfaceName in interfaceNames {
                        // Skip loopback and virtual interfaces
                        if interfaceName == "lo" || interfaceName.hasPrefix("veth")
                            || interfaceName.hasPrefix("docker")
                        {
                            continue
                        }

                        // Read interface type
                        let typePath = "/sys/class/net/\(interfaceName)/type"
                        guard
                            let typeStr = try? String(contentsOfFile: typePath, encoding: .utf8)
                                .trimmingCharacters(in: .whitespacesAndNewlines),
                            Int(typeStr) != nil
                        else {
                            continue
                        }

                        // Check if this is a Wendy interface: either by name or USB device association
                        let isWendy = interfaceName.contains("Wendy")
                            || wendyNetInterfaces.keys.contains(interfaceName)
                        guard isWendy else {
                            continue
                        }

                        // Read MAC address
                        let addressPath = "/sys/class/net/\(interfaceName)/address"
                        let macAddress = try? String(contentsOfFile: addressPath, encoding: .utf8)
                            .trimmingCharacters(in: .whitespacesAndNewlines)

                        // Read link speed (in Mbps, or -1 if unknown)
                        let speedPath = "/sys/class/net/\(interfaceName)/speed"
                        var linkSpeed: String? = nil
                        if let speedStr = try? String(contentsOfFile: speedPath, encoding: .utf8)
                            .trimmingCharacters(in: .whitespacesAndNewlines),
                            let speed = Int(speedStr), speed > 0
                        {
                            if speed >= 1000 {
                                let gbps = Double(speed) / 1000.0
                                if gbps == floor(gbps) {
                                    linkSpeed = "\(Int(gbps)) Gbps"
                                } else {
                                    linkSpeed = "\(gbps) Gbps"
                                }
                            } else {
                                linkSpeed = "\(speed) Mbps"
                            }
                        }

                        // Use the Wendy device product name as display name if available
                        let displayName = wendyNetInterfaces[interfaceName] ?? interfaceName

                        let ethernetInterface = EthernetInterface(
                            name: interfaceName,
                            displayName: displayName,
                            interfaceType: "Ethernet",
                            macAddress: macAddress,
                            linkSpeed: linkSpeed
                        )

                        interfaces.append(ethernetInterface)
                        logger.debug(
                            "Found Wendy Ethernet interface: \(interfaceName)",
                            metadata: [
                                "displayName": .string(displayName),
                                "speed": .string(linkSpeed ?? "unknown"),
                            ]
                        )
                    }

                    if interfaces.isEmpty {
                        logger.info("No Wendy Ethernet interfaces found.")
                    }
                } catch {
                    logger.error("Failed to list Ethernet interfaces: \(error)")
                }

                return interfaces
            #else
                logger.warning("Ethernet interface listing is not yet supported on Windows")
                return []
            #endif
        }

        /// Find network interfaces that belong to Wendy USB devices by walking sysfs.
        /// Returns a dictionary mapping interface name to the USB device's product name.
        private func findWendyUSBNetInterfaces() -> [String: String] {
            var result: [String: String] = [:]
            let sysPath = "/sys/bus/usb/devices"

            guard let entries = try? FileManager.default.contentsOfDirectory(atPath: sysPath) else {
                return result
            }

            for entry in entries {
                let devicePath = "\(sysPath)/\(entry)"

                // Read manufacturer and product to identify Wendy devices
                let manufacturer = (try? String(
                    contentsOfFile: "\(devicePath)/manufacturer", encoding: .utf8
                ))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                let product = (try? String(
                    contentsOfFile: "\(devicePath)/product", encoding: .utf8
                ))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""

                let combined = "\(manufacturer) \(product)"
                guard combined.localizedCaseInsensitiveContains("Wendy")
                    || combined.localizedCaseInsensitiveContains("WendyOS")
                else {
                    continue
                }

                // Walk child interfaces (e.g., 7-1:1.0, 7-1:1.1) looking for net/ subdirectory
                guard
                    let children = try? FileManager.default.contentsOfDirectory(atPath: devicePath)
                else {
                    continue
                }

                for child in children where child.hasPrefix(entry) {
                    let netPath = "\(devicePath)/\(child)/net"
                    if let netInterfaces = try? FileManager.default.contentsOfDirectory(
                        atPath: netPath
                    ) {
                        for iface in netInterfaces {
                            let displayName = product.isEmpty ? manufacturer : product
                            result[iface] = displayName
                            logger.debug(
                                "Mapped Wendy USB net interface",
                                metadata: [
                                    "interface": .string(iface),
                                    "usbDevice": .string(entry),
                                    "product": .string(product),
                                ]
                            )
                        }
                    }
                }
            }

            return result
        }

        public func findLANDevices() async throws -> [LANDevice] {
            let dns = try await DNSClient.connectMulticast(
                on: .singletonMultiThreadedEventLoopGroup
            ).get()
            let messages = try await dns.sendMulticastQuery(
                forHost: "_wendyos._udp.local",
                type: .any,
                timeout: timeout
            ).get()
            logger.debug(
                "Going to process answers to multicast query",
                metadata: ["answers": .stringConvertible(messages.count)]
            )

            var interfaces: [LANDevice] = []
            for message in messages {
                let srv = message.answers.compactMap { answer in
                    switch answer {
                    case .srv(let srv):
                        return srv
                    default:
                        return nil
                    }
                }.first

                let txt = message.answers.compactMap { answer in
                    switch answer {
                    case .txt(let txt):
                        return txt
                    default:
                        return nil
                    }
                }.first

                guard let srv = srv else {
                    logger.debug("Got no SRV answer")
                    continue
                }

                let id = txt?.resource.values.values.first ?? "WendyOS Device"

                let lanDevice = LANDevice(
                    id: id,
                    displayName: id,
                    hostname: srv.resource.domainName.string,
                    port: Int(srv.resource.port),
                    interfaceType: "LAN",
                    isWendyDevice: true
                )

                // Prevent duplicates
                if !interfaces.contains(where: { $0.id == id || $0.hostname == lanDevice.hostname })
                {
                    interfaces.append(lanDevice)
                }
            }

            return interfaces
        }

        /// Discover LAN devices reachable via Wendy USB network interfaces.
        /// Uses IPv6 NDP to find peer link-local addresses on USB-connected devices,
        /// then probes the agent port to create connectable LANDevice entries.
        public func findUSBLANDevices() async -> [LANDevice] {
            #if os(Linux)
                let wendyNetInterfaces = findWendyUSBNetInterfaces()
                guard !wendyNetInterfaces.isEmpty else { return [] }

                var devices: [LANDevice] = []

                for (interfaceName, productName) in wendyNetInterfaces {
                    // Discover peer IPv6 link-local addresses via NDP.
                    // Send an ICMPv6 multicast ping to ff02::1 (all-nodes) on this interface,
                    // then read the neighbor table for link-local addresses.
                    let peerAddresses = await discoverPeerLinkLocal(on: interfaceName)

                    for peerAddr in peerAddresses {
                        // Use the address with scope ID so gRPC can route to it
                        let scopedAddress = "\(peerAddr)%\(interfaceName)"
                        let device = LANDevice(
                            id: productName,
                            displayName: productName,
                            hostname: scopedAddress,
                            port: 50051,
                            interfaceType: "USB",
                            isWendyDevice: true
                        )
                        devices.append(device)
                        logger.info(
                            "Found Wendy device via USB interface",
                            metadata: [
                                "interface": .string(interfaceName),
                                "address": .string(scopedAddress),
                            ]
                        )
                    }
                }

                return devices
            #else
                return []
            #endif
        }

        #if os(Linux)
        /// Discover peer IPv6 link-local addresses on a network interface via NDP.
        private func discoverPeerLinkLocal(on interfaceName: String) async -> [String] {
            // First, trigger NDP by pinging the all-nodes multicast address
            do {
                let pingResult = try await Subprocess.run(
                    .name("ping"),
                    arguments: ["-6", "-c", "1", "-W", "1", "-I", interfaceName, "ff02::1"],
                    output: .discarded,
                    error: .discarded
                )
                _ = pingResult  // We don't care about the result, just triggering NDP
            } catch {
                logger.debug("NDP ping failed on \(interfaceName): \(error)")
            }

            // Brief wait for NDP to complete
            try? await Task.sleep(for: .milliseconds(500))

            // Read our own link-local address to exclude it
            var ownAddresses = Set<String>()
            if let ifinet6 = try? String(contentsOfFile: "/proc/net/if_inet6", encoding: .utf8) {
                for line in ifinet6.split(separator: "\n") {
                    let parts = line.split(separator: " ", omittingEmptySubsequences: true)
                    // Format: addr device_number prefix_length scope flags iface_name
                    guard parts.count >= 6 else { continue }
                    let iface = String(parts[5])
                    let scope = String(parts[3])
                    guard iface == interfaceName, scope == "20" else { continue } // 20 = link scope
                    // Convert compact hex to IPv6 format
                    let hex = String(parts[0])
                    if let formatted = Self.formatIPv6FromHex(hex) {
                        ownAddresses.insert(formatted)
                    }
                }
            }

            // Read the IPv6 neighbor table to find peer addresses
            var peerAddresses: [String] = []
            do {
                let result = try await Subprocess.run(
                    .name("ip"),
                    arguments: ["-6", "neigh", "show", "dev", interfaceName],
                    output: .string(limit: 10_000),
                    error: .discarded
                )
                if let output = result.standardOutput {
                    for line in output.split(separator: "\n") {
                        let parts = line.split(separator: " ", omittingEmptySubsequences: true)
                        guard let addr = parts.first else { continue }
                        let addrStr = String(addr)
                        // Only include link-local addresses (fe80::) that aren't ours
                        if addrStr.lowercased().hasPrefix("fe80:") && !ownAddresses.contains(addrStr) {
                            peerAddresses.append(addrStr)
                        }
                    }
                }
            } catch {
                logger.debug("Failed to read IPv6 neighbor table for \(interfaceName): \(error)")
            }

            return peerAddresses
        }

        /// Convert a 32-char hex string from /proc/net/if_inet6 to standard IPv6 notation.
        private static func formatIPv6FromHex(_ hex: String) -> String? {
            guard hex.count == 32 else { return nil }
            var groups: [String] = []
            var index = hex.startIndex
            for _ in 0..<8 {
                let end = hex.index(index, offsetBy: 4)
                let group = String(hex[index..<end])
                // Remove leading zeros for compact representation
                let trimmed = group.replacingOccurrences(
                    of: "^0+", with: "", options: .regularExpression
                )
                groups.append(trimmed.isEmpty ? "0" : trimmed)
                index = end
            }
            return groups.joined(separator: ":")
        }
        #endif

        public func findBluetoothDevices(
            resolveAgentVersion: Bool = false
        ) async throws -> [BluetoothDevice] {
            return []
        }
    }
#endif
