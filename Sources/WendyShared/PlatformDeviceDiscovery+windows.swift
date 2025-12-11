#if os(Windows)
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
            logger.debug("Listing USB devices on Windows not supported yet")
            return []
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
            logger.debug("Listing Ethernet interfaces on Windows not supported yet")
            return []
        }

        public func findLANDevices() async throws -> [LANDevice] {
            let dns = try await DNSClient.connectMulticast(
                on: .singletonMultiThreadedEventLoopGroup
            ).get()
            async let wendyPTR = try? await dns.sendQuery(
                forHost: "_wendyos._udp.local",
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
    }
#endif
