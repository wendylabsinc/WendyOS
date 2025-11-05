#if os(Linux)
    import DNSClient
    import Foundation
    import Logging
    import Subprocess

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let logger: Logger

        public init(
            logger: Logger
        ) {
            self.logger = logger
        }

        public func findUSBDevices() async -> [USBDevice] {
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
                                            "vendorId": .string(String(format: "0x%04X", vendorId)),
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
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
            logger.error("Listing Ethernet interfaces on Linux is not implemented")
            let interfaces: [EthernetInterface] = []
            return interfaces
        }

        public func findLANDevices() async throws -> [LANDevice] {
            let dns = try await DNSClient.connectMulticast(
                on: .singletonMultiThreadedEventLoopGroup
            ).get()
            async let wendyPTR = try? await dns.sendQuery(
                forHost: "_wendy._udp.local",
                type: .any,
                timeout: .seconds(5)
            ).get()
            async let edgePTR = try? await dns.sendQuery(
                forHost: "_edgeos._udp.local",
                type: .any,
                timeout: .seconds(5)
            ).get()
            let messages = await [wendyPTR, edgePTR]
            logger.debug(
                "Going to process answers to PTR query",
                metadata: ["answers": .stringConvertible(messages.count)]
            )

            var interfaces: [LANDevice] = []
            for case .some(let message) in messages {
                let ptr = message.answers.compactMap { answer in
                    switch answer {
                    case .ptr(let ptr):
                        return ptr
                    default:
                        return nil
                    }
                }.first

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
                    logger.debug("Got no SRV answer to PTR query")
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
    }
#endif
