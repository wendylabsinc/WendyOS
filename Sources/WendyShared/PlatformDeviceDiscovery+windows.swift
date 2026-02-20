#if os(Windows)
    import Foundation
    import Logging
    import NIOCore
    import WinSDK

    public struct PlatformDeviceDiscovery: DeviceDiscovery {
        private let logger: Logger
        package var timeout: NIOCore.TimeAmount = .seconds(5)

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

        public func findBluetoothDevices(
            resolveAgentVersion: Bool
        ) async throws -> [BluetoothDevice] {
            logger.debug("Listing Bluetooth devices on Windows not supported yet")
            return []
        }

        public func findLANDevices() async throws -> [LANDevice] {
            logger.debug("Starting mDNS discovery using Windows native API")

            // Use Windows DNS-SD API via DnsServiceBrowse
            return try await withCheckedThrowingContinuation { continuation in
                Task {
                    do {
                        let devices = try await browseMDNSServices()
                        continuation.resume(returning: devices)
                    } catch {
                        continuation.resume(throwing: error)
                    }
                }
            }
        }

        /// Browse for mDNS services using Windows native multicast
        private func browseMDNSServices() async throws -> [LANDevice] {
            let timeoutSeconds = Int(timeout.nanoseconds / 1_000_000_000)
            var devices: [LANDevice] = []

            // Query for both service types
            for serviceType in ["_wendyos._udp.local", "_edgeos._udp.local"] {
                logger.debug("Querying mDNS service", metadata: ["service": "\(serviceType)"])

                if let found = try? await queryMDNSService(serviceType, timeout: timeoutSeconds) {
                    for device in found {
                        if !devices.contains(where: {
                            $0.id == device.id || $0.hostname == device.hostname
                        }) {
                            devices.append(device)
                        }
                    }
                }
            }

            logger.debug("mDNS discovery complete", metadata: ["found": "\(devices.count)"])
            return devices
        }

        /// Query a specific mDNS service type using raw sockets
        private func queryMDNSService(
            _ serviceType: String,
            timeout: Int
        ) async throws -> [LANDevice] {
            // Create UDP socket
            let sock = socket(AF_INET, SOCK_DGRAM, Int32(IPPROTO_UDP.rawValue))
            guard sock != INVALID_SOCKET else {
                logger.error("Failed to create socket")
                return []
            }
            defer { closesocket(sock) }

            // Enable address reuse
            var reuseAddr: Int32 = 1
            setsockopt(sock, SOL_SOCKET, SO_REUSEADDR, &reuseAddr, Int32(MemoryLayout<Int32>.size))

            // Bind to mDNS port
            var bindAddr = sockaddr_in()
            bindAddr.sin_family = ADDRESS_FAMILY(AF_INET)
            bindAddr.sin_port = UInt16(5353).bigEndian
            bindAddr.sin_addr.S_un.S_addr = INADDR_ANY

            let bindResult = withUnsafePointer(to: &bindAddr) { ptr in
                ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
                    bind(sock, sockPtr, Int32(MemoryLayout<sockaddr_in>.size))
                }
            }

            if bindResult == SOCKET_ERROR {
                logger.debug("Failed to bind to port 5353, trying ephemeral port")
                bindAddr.sin_port = 0
                _ = withUnsafePointer(to: &bindAddr) { ptr in
                    ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
                        bind(sock, sockPtr, Int32(MemoryLayout<sockaddr_in>.size))
                    }
                }
            }

            // Join multicast group on all suitable interfaces
            let interfaces = getPrivateLANInterfaces()
            for interfaceAddr in interfaces {
                var mreq = ip_mreq()
                mreq.imr_multiaddr.S_un.S_addr = inet_addr("224.0.0.251")
                mreq.imr_interface.S_un.S_addr = inet_addr(interfaceAddr)

                let joinResult = setsockopt(
                    sock,
                    Int32(IPPROTO_IP),
                    IP_ADD_MEMBERSHIP,
                    &mreq,
                    Int32(MemoryLayout<ip_mreq>.size)
                )
                if joinResult == 0 {
                    logger.debug(
                        "Joined multicast on interface",
                        metadata: ["ip": "\(interfaceAddr)"]
                    )
                }
            }

            // Set socket timeout
            var tv: Int32 = Int32(timeout * 1000)  // milliseconds
            setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, Int32(MemoryLayout<Int32>.size))

            // Build and send mDNS query
            let query = buildMDNSQuery(for: serviceType)
            var destAddr = sockaddr_in()
            destAddr.sin_family = ADDRESS_FAMILY(AF_INET)
            destAddr.sin_port = UInt16(5353).bigEndian
            destAddr.sin_addr.S_un.S_addr = inet_addr("224.0.0.251")

            // Send query on each interface
            for interfaceAddr in interfaces {
                var ifAddr = in_addr()
                ifAddr.S_un.S_addr = inet_addr(interfaceAddr)
                setsockopt(
                    sock,
                    Int32(IPPROTO_IP),
                    IP_MULTICAST_IF,
                    &ifAddr,
                    Int32(MemoryLayout<in_addr>.size)
                )

                _ = query.withUnsafeBytes { queryPtr in
                    withUnsafePointer(to: &destAddr) { destPtr in
                        destPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
                            sendto(
                                sock,
                                queryPtr.baseAddress,
                                Int32(query.count),
                                0,
                                sockPtr,
                                Int32(MemoryLayout<sockaddr_in>.size)
                            )
                        }
                    }
                }
                logger.debug("Sent mDNS query via", metadata: ["interface": "\(interfaceAddr)"])
            }

            // Collect responses
            var devices: [LANDevice] = []
            let bufferSize = 4096
            let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: bufferSize)
            defer { buffer.deallocate() }
            let deadline = Date().addingTimeInterval(Double(timeout))

            while Date() < deadline {
                var srcAddr = sockaddr_in()
                var srcAddrLen = Int32(MemoryLayout<sockaddr_in>.size)

                let bytesReceived = withUnsafeMutablePointer(to: &srcAddr) { srcPtr in
                    srcPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
                        recvfrom(sock, buffer, Int32(bufferSize), 0, sockPtr, &srcAddrLen)
                    }
                }

                if bytesReceived > 0 {
                    let responseData = Array(
                        UnsafeBufferPointer(start: buffer, count: Int(bytesReceived))
                    )
                    logger.trace("Received mDNS response", metadata: ["bytes": "\(bytesReceived)"])
                    if let device = parseMDNSResponse(responseData, serviceType: serviceType) {
                        if !devices.contains(where: { $0.id == device.id }) {
                            devices.append(device)
                            logger.debug(
                                "Found device",
                                metadata: ["id": "\(device.id)", "hostname": "\(device.hostname)"]
                            )
                        }
                    }
                } else {
                    break  // Timeout or error
                }
            }

            return devices
        }

        /// Get list of private LAN interface IP addresses
        private func getPrivateLANInterfaces() -> [String] {
            var interfaces: [String] = []

            guard let devices = try? System.enumerateDevices() else {
                return ["0.0.0.0"]  // Fallback to any
            }

            for device in devices {
                guard device.multicastSupported else { continue }
                guard let ipString = device.address?.ipAddress else { continue }

                // Skip virtual adapters
                let nameLower = device.name.lowercased()
                if nameLower.contains("vethernet") || nameLower.contains("virtualbox")
                    || nameLower.contains("vmware") || nameLower.contains("virtual")
                {
                    continue
                }

                // Check for private subnets
                let parts = ipString.split(separator: ".").compactMap { UInt8($0) }
                guard parts.count == 4 else { continue }

                let byte1 = parts[0]
                let byte2 = parts[1]

                let isPrivateLAN =
                    (byte1 == 192 && byte2 == 168) || byte1 == 10
                    || (byte1 == 172 && byte2 >= 16 && byte2 <= 31)

                if isPrivateLAN {
                    interfaces.append(ipString)
                }
            }

            return interfaces.isEmpty ? ["0.0.0.0"] : interfaces
        }

        /// Build an mDNS query packet
        private func buildMDNSQuery(for serviceName: String) -> [UInt8] {
            var packet: [UInt8] = []

            // Header
            packet += [0x00, 0x00]  // Transaction ID
            packet += [0x00, 0x00]  // Flags (standard query)
            packet += [0x00, 0x01]  // Questions: 1
            packet += [0x00, 0x00]  // Answer RRs: 0
            packet += [0x00, 0x00]  // Authority RRs: 0
            packet += [0x00, 0x00]  // Additional RRs: 0

            // Question: encode service name as DNS labels
            let labels = serviceName.split(separator: ".")
            for label in labels {
                let labelBytes = Array(label.utf8)
                packet.append(UInt8(labelBytes.count))
                packet += labelBytes
            }
            packet.append(0x00)  // End of name

            packet += [0x00, 0xFF]  // Type: ANY
            packet += [0x00, 0x01]  // Class: IN

            return packet
        }

        /// Parse an mDNS response packet
        private func parseMDNSResponse(_ data: [UInt8], serviceType: String) -> LANDevice? {
            guard data.count > 12 else { return nil }

            // Check if this is a response (bit 15 of flags)
            let flags = UInt16(data[2]) << 8 | UInt16(data[3])
            guard (flags & 0x8000) != 0 else { return nil }  // Not a response

            let answerCount = UInt16(data[6]) << 8 | UInt16(data[7])
            guard answerCount > 0 else { return nil }

            // Parse answers - look for SRV and TXT records
            var offset = 12

            // Skip questions
            let questionCount = UInt16(data[4]) << 8 | UInt16(data[5])
            for _ in 0..<questionCount {
                offset = skipDNSName(data, offset: offset)
                offset += 4  // Type + Class
            }

            var hostname: String?
            var port: Int?
            var txtRecords: [String: String] = [:]
            var isWendyService = false

            // Parse answers
            for _ in 0..<answerCount {
                guard offset < data.count else { break }

                let recordName = readDNSName(data, offset: offset)
                let nameEnd = skipDNSName(data, offset: offset)
                guard nameEnd + 10 <= data.count else { break }

                let recordType = UInt16(data[nameEnd]) << 8 | UInt16(data[nameEnd + 1])
                logger.trace(
                    "Parsing record",
                    metadata: ["name": "\(recordName)", "type": "\(recordType)"]
                )

                // Check if this record is for our service type
                if recordName.contains("_wendyos") || recordName.contains("_edgeos") {
                    isWendyService = true
                    logger.trace("Matched WendyOS/EdgeOS service")
                }

                let dataLength = UInt16(data[nameEnd + 8]) << 8 | UInt16(data[nameEnd + 9])
                let recordDataStart = nameEnd + 10

                if recordType == 33 {  // SRV record
                    guard recordDataStart + 6 < data.count else { break }
                    port = Int(
                        UInt16(data[recordDataStart + 4]) << 8 | UInt16(data[recordDataStart + 5])
                    )
                    hostname = readDNSName(data, offset: recordDataStart + 6)
                    logger.trace(
                        "SRV record",
                        metadata: ["hostname": "\(hostname ?? "nil")", "port": "\(port ?? 0)"]
                    )
                } else if recordType == 16 {  // TXT record
                    let parsed = parseTXTRecords(
                        data,
                        offset: recordDataStart,
                        length: Int(dataLength)
                    )
                    for (key, value) in parsed {
                        txtRecords[key] = value
                    }
                    logger.trace("TXT record", metadata: ["records": "\(txtRecords)"])
                }

                offset = recordDataStart + Int(dataLength)
            }

            // Only return WendyOS/EdgeOS devices
            if !isWendyService {
                logger.trace(
                    "Skipping non-WendyOS response",
                    metadata: ["hostname": "\(hostname ?? "unknown")"]
                )
                return nil
            }
            guard let host = hostname, let p = port else { return nil }

            // Extract device info from TXT records
            let identity = LANDevice.extractIdentity(
                from: txtRecords,
                fallbackId: host
            )

            return LANDevice(
                id: identity.id,
                displayName: identity.displayName,
                hostname: host,
                port: p,
                interfaceType: "LAN",
                isWendyDevice: true
            )
        }

        private func skipDNSName(_ data: [UInt8], offset: Int) -> Int {
            var pos = offset
            while pos < data.count {
                let len = data[pos]
                if len == 0 {
                    return pos + 1
                } else if (len & 0xC0) == 0xC0 {
                    return pos + 2  // Compression pointer
                } else {
                    pos += Int(len) + 1
                }
            }
            return pos
        }

        private func readDNSName(_ data: [UInt8], offset: Int) -> String {
            var parts: [String] = []
            var pos = offset
            var maxJumps = 10

            while pos < data.count && maxJumps > 0 {
                let len = data[pos]
                if len == 0 {
                    break
                } else if (len & 0xC0) == 0xC0 {
                    // Compression pointer
                    guard pos + 1 < data.count else { break }
                    let ptr = Int(UInt16(len & 0x3F) << 8 | UInt16(data[pos + 1]))
                    pos = ptr
                    maxJumps -= 1
                } else {
                    let start = pos + 1
                    let end = start + Int(len)
                    guard end <= data.count else { break }
                    if let label = String(bytes: data[start..<end], encoding: .utf8) {
                        parts.append(label)
                    }
                    pos = end
                }
            }

            return parts.joined(separator: ".")
        }

        /// Parse TXT record into key=value pairs
        private func parseTXTRecords(_ data: [UInt8], offset: Int, length: Int) -> [String: String]
        {
            var records: [String: String] = [:]
            guard offset < data.count else { return records }
            let end = min(offset + length, data.count)
            var pos = offset

            while pos < end {
                let strLen = Int(data[pos])
                guard strLen > 0 else {
                    pos += 1
                    continue
                }
                let strStart = pos + 1
                let strEnd = strStart + strLen
                guard strEnd <= end else { break }

                if let str = String(bytes: data[strStart..<strEnd], encoding: .utf8) {
                    // Parse key=value format
                    if let eqIndex = str.firstIndex(of: "=") {
                        let key = String(str[str.startIndex..<eqIndex])
                        let value = String(str[str.index(after: eqIndex)...])
                        records[key] = value
                    } else {
                        // No "=" - treat whole string as a flag
                        records[str] = "true"
                    }
                }
                pos = strEnd
            }
            return records
        }
    }
#endif
