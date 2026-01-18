#if !os(macOS) && !os(Linux) && !os(Windows)
    import Foundation
    import Logging

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let logger: Logger

        public init(logger: Logger) {
            self.logger = logger
        }

        public func findUSBDevices() async -> [USBDevice] {
            logger.warning("Device listing is not supported on this platform")
            return []
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
            logger.warning("Interface listing is not supported on this platform")
            return []
        }

        public func findLANDevices() async throws -> [LANDevice] {
            logger.warning("LAN device listing is not supported on this platform")
            return []
        }
    }
#endif
