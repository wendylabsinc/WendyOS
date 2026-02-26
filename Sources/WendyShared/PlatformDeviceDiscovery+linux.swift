#if os(Linux)
    import DNSClient
    import Foundation
    import Logging
    import NIOCore
    import Subprocess

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let logger: Logger
        package var timeout: NIOCore.TimeAmount = .seconds(5)

        public init(
            logger: Logger
        ) {
            self.logger = logger
        }

        /// Info about a Wendy USB device read from sysfs.
        private struct SysfsUSBDevice {
            let sysfsEntry: String
            let manufacturer: String
            let product: String
            let vendorId: Int
            let productId: Int
            let serialNumber: String?
            let usbVersion: String?

            var displayName: String {
                product.isEmpty ? manufacturer : product
            }
        }

        /// Enumerate Wendy USB devices from /sys/bus/usb/devices.
        private func enumerateWendyUSBDevices() -> [SysfsUSBDevice] {
            let sysPath = "/sys/bus/usb/devices"
            guard let entries = try? FileManager.default.contentsOfDirectory(atPath: sysPath) else {
                return []
            }

            var devices: [SysfsUSBDevice] = []
            for entry in entries {
                let devicePath = "\(sysPath)/\(entry)"

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

                devices.append(SysfsUSBDevice(
                    sysfsEntry: entry,
                    manufacturer: manufacturer,
                    product: product,
                    vendorId: vendorId,
                    productId: productId,
                    serialNumber: serialNumber,
                    usbVersion: usbVersion
                ))
            }
            return devices
        }

        public func findUSBDevices() async -> [USBDevice] {
            logger.info("Listing USB devices on Linux")

            let sysfsDevices = enumerateWendyUSBDevices()
            let devices = sysfsDevices.map { dev in
                USBDevice(
                    name: dev.displayName,
                    vendorId: dev.vendorId,
                    productId: dev.productId,
                    usbVersion: dev.usbVersion,
                    serialNumber: dev.serialNumber
                )
            }

            for (dev, sysfs) in zip(devices, sysfsDevices) {
                logger.info(
                    "Found Wendy USB device: \(dev.name)",
                    metadata: [
                        "vendorId": .string(dev.vendorId),
                        "productId": .string(dev.productId),
                        "manufacturer": .string(sysfs.manufacturer),
                        "sysfs": .string(sysfs.sysfsEntry),
                    ]
                )
            }

            if devices.isEmpty {
                logger.info("No Wendy USB devices found.")
            }

            return devices
        }

        public func findEthernetInterfaces() async -> [EthernetInterface] {
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
        }

        /// Find network interfaces that belong to Wendy USB devices by walking sysfs.
        /// Returns a dictionary mapping interface name to the USB device's product name.
        private func findWendyUSBNetInterfaces() -> [String: String] {
            let sysPath = "/sys/bus/usb/devices"
            var result: [String: String] = [:]

            for dev in enumerateWendyUSBDevices() {
                let devicePath = "\(sysPath)/\(dev.sysfsEntry)"
                guard
                    let children = try? FileManager.default.contentsOfDirectory(atPath: devicePath)
                else {
                    continue
                }

                // Walk child interfaces (e.g., 7-1:1.0, 7-1:1.1) looking for net/ subdirectory
                for child in children where child.hasPrefix(dev.sysfsEntry) {
                    let netPath = "\(devicePath)/\(child)/net"
                    if let netInterfaces = try? FileManager.default.contentsOfDirectory(
                        atPath: netPath
                    ) {
                        for iface in netInterfaces {
                            result[iface] = dev.displayName
                            logger.debug(
                                "Mapped Wendy USB net interface",
                                metadata: [
                                    "interface": .string(iface),
                                    "usbDevice": .string(dev.sysfsEntry),
                                    "product": .string(dev.product),
                                ]
                            )
                        }
                    }
                }
            }

            return result
        }

        public func findLANDevices() async throws -> [LANDevice] {
            var devices: [LANDevice] = []
            try await withLANDeviceDiscovery { device in
                devices.append(device)
            }
            return devices
        }

        public func withLANDeviceDiscovery(
            _ handler: (LANDevice) async throws -> Void
        ) async throws {
            let duration = Duration.seconds(Int64(timeout.nanoseconds / 1_000_000_000))
            var seen = Set<String>()

            for await entry in MdnsBrowser.browse(
                serviceType: "_wendyos._udp.local.",
                timeout: duration,
                logger: logger
            ) {
                try Task.checkCancellation()
                let id = entry.text.values.first ?? entry.name
                let hostname = entry.hostname

                // Deduplicate by hostname
                guard !seen.contains(hostname) else { continue }
                seen.insert(hostname)

                let device = LANDevice(
                    id: id,
                    displayName: id,
                    hostname: hostname,
                    port: Int(entry.port),
                    interfaceType: "LAN",
                    isWendyDevice: true
                )
                try await handler(device)
            }
        }

        /// Discover LAN devices reachable via Wendy USB network interfaces.
        /// Uses IPv6 NDP to find peer link-local addresses on USB-connected devices,
        /// then probes the agent port to create connectable LANDevice entries.
        public func findUSBLANDevices() async -> [LANDevice] {
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
                        id: "\(productName)@\(scopedAddress)",
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
        }

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
                    if let formatted = formatIPv6FromHex(hex) {
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
                    // ip -6 neigh format: "fe80::1 dev usb0 lladdr xx:xx:xx REACHABLE"
                    // Filter out FAILED/INCOMPLETE entries that are no longer reachable
                    let unreachableStates: Set<String> = ["FAILED", "INCOMPLETE", "NONE"]
                    for line in output.split(separator: "\n") {
                        let parts = line.split(separator: " ", omittingEmptySubsequences: true)
                        guard let addr = parts.first else { continue }
                        let addrStr = String(addr)
                        // Check neighbor state (last field)
                        if let state = parts.last, unreachableStates.contains(String(state)) {
                            continue
                        }
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

        // formatIPv6FromHex is defined in IPv6Utils.swift as a package-level function.

        public func findBluetoothDevices(
            resolveAgentVersion: Bool = false
        ) async throws -> [BluetoothDevice] {
            return []
        }
    }
#endif
