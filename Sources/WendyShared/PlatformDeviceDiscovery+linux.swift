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
                    let result = try await Subprocess.run(
                        Subprocess.Executable.path("/usr/bin/lsusb"),
                        arguments: Subprocess.Arguments([String]()),
                        output: .string(limit: .max)
                    )
                    let output = result.standardOutput ?? ""

                    for line in output.split(separator: "\n") {
                        let deviceInfo = String(line)
                        logger.debug("Found USB device: \(deviceInfo)")

                        // Parse the lsusb output format: "Bus XXX Device XXX: ID VVVV:PPPP Manufacturer Device"
                        if deviceInfo.contains("Wendy") {
                            // Extract vendor and product IDs
                            if let idRange = deviceInfo.range(
                                of: "ID \\S+",
                                options: String.CompareOptions.regularExpression
                            ) {
                                let idStr = deviceInfo[idRange].dropFirst(3)  // Drop "ID "
                                let parts = idStr.split(separator: ":")

                                if parts.count == 2,
                                    let vendorId = Int(parts[0], radix: 16),
                                    let productId = Int(parts[1], radix: 16)
                                {

                                    // Extract name - everything after the ID part
                                    let nameStartIndex = deviceInfo.index(
                                        idRange.upperBound,
                                        offsetBy: 1
                                    )
                                    if nameStartIndex < deviceInfo.endIndex {
                                        let name = String(deviceInfo[nameStartIndex...])
                                            .trimmingCharacters(in: .whitespaces)

                                        devices.append(
                                            USBDevice(
                                                name: name,
                                                vendorId: vendorId,
                                                productId: productId
                                            )
                                        )

                                        logger.info(
                                            "Found Wendy USB device: \(name)",
                                            metadata: [
                                                "vendorId": .string(
                                                    String(format: "0x%04X", vendorId)
                                                ),
                                                "productId": .string(
                                                    String(format: "0x%04X", productId)
                                                ),
                                            ]
                                        )
                                    }
                                }
                            }
                        }
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
                            // Format speed as human-readable string
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

                        // Only collect interfaces containing "Wendy" in their name
                        if !interfaceName.contains("Wendy") {
                            continue
                        }

                        let ethernetInterface = EthernetInterface(
                            name: interfaceName,
                            displayName: interfaceName,
                            interfaceType: "Ethernet",
                            macAddress: macAddress,
                            linkSpeed: linkSpeed
                        )

                        interfaces.append(ethernetInterface)
                        logger.debug(
                            "Found Wendy Ethernet interface: \(interfaceName)",
                            metadata: [
                                "speed": .string(linkSpeed ?? "unknown")
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

        public func findBluetoothDevices(
            resolveAgentVersion: Bool = false
        ) async throws -> [BluetoothDevice] {
            return []
        }
    }
#endif
