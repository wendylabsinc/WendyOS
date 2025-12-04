#if os(macOS)
    import Foundation
    import SystemConfiguration

    /// Protocol that abstracts SystemConfiguration network interface operations to allow for dependency injection and testing
    public protocol NetworkInterfaceProvider: Sendable {
        /// Gets all network interfaces
        func copyAllNetworkInterfaces() -> [SCNetworkInterface]?

        /// Gets the interface type of a network interface
        func getInterfaceType(interface: SCNetworkInterface) -> String?

        /// Gets the BSD name of a network interface
        func getBSDName(interface: SCNetworkInterface) -> String?

        /// Gets the localized display name of a network interface
        func getLocalizedDisplayName(interface: SCNetworkInterface) -> String?

        /// Gets the hardware address (MAC) of a network interface
        func getHardwareAddressString(interface: SCNetworkInterface) -> String?

        /// Gets the link speed of a network interface (e.g., "1 Gbps", "100 Mbps")
        func getLinkSpeed(interfaceName: String) -> String?
    }

    /// Default implementation that uses the real SystemConfiguration APIs
    public final class DefaultNetworkInterfaceProvider: NetworkInterfaceProvider {
        public init() {}

        public func copyAllNetworkInterfaces() -> [SCNetworkInterface]? {
            return SCNetworkInterfaceCopyAll() as? [SCNetworkInterface]
        }

        public func getInterfaceType(interface: SCNetworkInterface) -> String? {
            return SCNetworkInterfaceGetInterfaceType(interface) as? String
        }

        public func getBSDName(interface: SCNetworkInterface) -> String? {
            return SCNetworkInterfaceGetBSDName(interface) as? String
        }

        public func getLocalizedDisplayName(interface: SCNetworkInterface) -> String? {
            return SCNetworkInterfaceGetLocalizedDisplayName(interface) as? String
        }

        public func getHardwareAddressString(interface: SCNetworkInterface) -> String? {
            return SCNetworkInterfaceGetHardwareAddressString(interface) as? String
        }

        public func getLinkSpeed(interfaceName: String) -> String? {
            // Use ifconfig to get media information
            let process = Process()
            process.executableURL = URL(fileURLWithPath: "/sbin/ifconfig")
            process.arguments = [interfaceName]

            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = FileHandle.nullDevice

            do {
                try process.run()
                process.waitUntilExit()

                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                guard let output = String(data: data, encoding: .utf8) else {
                    return nil
                }

                // Parse the media line, e.g., "media: autoselect (1000baseT <full-duplex>)"
                // or "media: autoselect (100baseTX <full-duplex>)"
                return Self.parseMediaSpeed(from: output)
            } catch {
                return nil
            }
        }

        /// Parses the link speed from ifconfig output
        private static func parseMediaSpeed(from output: String) -> String? {
            // Look for the media line
            guard
                let mediaLine = output.split(separator: "\n")
                    .first(where: { $0.contains("media:") })
            else {
                return nil
            }

            let mediaString = String(mediaLine)

            // Check for autoselect with no active media (link down)
            if mediaString.contains("autoselect") && !mediaString.contains("(") {
                return nil
            }

            // Regex captures: (number)(optional G)base
            guard
                let regex = try? NSRegularExpression(
                    pattern: #"(\d+\.?\d*)(G)?base"#,
                    options: .caseInsensitive
                )
            else {
                return nil
            }

            let range = NSRange(mediaString.startIndex..., in: mediaString)
            guard let match = regex.firstMatch(in: mediaString, options: [], range: range),
                let numberRange = Range(match.range(at: 1), in: mediaString),
                let speedValue = Double(mediaString[numberRange])
            else {
                return nil
            }

            // Check if "G" modifier is present (group 2)
            let hasGigabitModifier = match.range(at: 2).location != NSNotFound

            if hasGigabitModifier {
                // Already in Gbps (e.g., "10Gbase" -> 10 Gbps)
                return formatSpeed(gbps: speedValue)
            } else if speedValue >= 1000 {
                // Convert Mbps to Gbps (e.g., "1000base" -> 1 Gbps)
                return formatSpeed(gbps: speedValue / 1000)
            } else {
                // Already in Mbps (e.g., "100base" -> 100 Mbps)
                return formatSpeed(mbps: speedValue)
            }
        }

        // Format whole values (e.g. 10.0 Gbps -> 10 Gbps)

        private static func formatSpeed(gbps: Double) -> String {
            if gbps == floor(gbps) {
                return "\(Int(gbps)) Gbps"
            } else {
                return "\(gbps) Gbps"
            }
        }

        private static func formatSpeed(mbps: Double) -> String {
            if mbps == floor(mbps) {
                return "\(Int(mbps)) Mbps"
            } else {
                return "\(mbps) Mbps"
            }
        }
    }
#endif
