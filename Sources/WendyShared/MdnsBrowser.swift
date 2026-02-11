#if os(Linux)
    import CMdns
    import Foundation
    import Logging

    #if canImport(Glibc)
        import Glibc
    #elseif canImport(Musl)
        import Musl
    #endif

    /// A discovered mDNS service entry.
    package struct MdnsServiceEntry: Sendable {
        package let name: String
        package let hostname: String
        package let port: UInt16
        package let addresses: [String]
        package let text: [String: String]
    }

    /// Browse for mDNS services on the local network using the C mdns library.
    package enum MdnsBrowser {

        /// Browse for services of the given type, yielding entries as they are discovered.
        package static func browse(
            serviceType: String,
            timeout: Duration,
            logger: Logger
        ) -> AsyncStream<MdnsServiceEntry> {
            AsyncStream { continuation in
                let task = Task.detached {
                    do {
                        try Self.performBrowse(
                            serviceType: serviceType,
                            timeout: timeout,
                            logger: logger,
                            continuation: continuation
                        )
                    } catch {
                        logger.error("mDNS browse failed: \(error)")
                    }
                    continuation.finish()
                }
                continuation.onTermination = { _ in
                    task.cancel()
                }
            }
        }

        // MARK: - Internal

        private static func performBrowse(
            serviceType: String,
            timeout: Duration,
            logger: Logger,
            continuation: AsyncStream<MdnsServiceEntry>.Continuation
        ) throws {
            // 1. Open sockets on all multicast-capable interfaces
            let sockets = openMulticastSockets(logger: logger)
            defer {
                for sock in sockets {
                    cmdns_socket_close(sock)
                }
            }

            guard !sockets.isEmpty else {
                logger.warning("No multicast-capable interfaces found")
                return
            }

            logger.debug(
                "Opened mDNS sockets",
                metadata: ["count": "\(sockets.count)"]
            )

            // 2. Send PTR queries on all sockets
            var sendBuf = [UInt8](repeating: 0, count: 2048)
            for sock in sockets {
                let result = serviceType.withCString { cstr in
                    cmdns_query_send(
                        sock,
                        UInt16(CMDNS_RECORDTYPE_PTR),
                        cstr,
                        serviceType.utf8.count,
                        &sendBuf,
                        sendBuf.count,
                        0
                    )
                }
                if result < 0 {
                    logger.warning(
                        "Failed to send PTR query",
                        metadata: ["socket": "\(sock)"]
                    )
                }
            }

            // 3. Recv loop using select()-based waiting per socket
            let deadline = ContinuousClock.now + timeout
            var recvBuf = [UInt8](repeating: 0, count: 4096)
            var seen = Set<String>()

            // Context for the C callback
            let context = BrowseContext(serviceType: serviceType)
            let ctxPtr = Unmanaged.passUnretained(context).toOpaque()

            while !Task.isCancelled {
                let remaining = deadline - ContinuousClock.now
                let remainingMs = Int32(
                    max(
                        0,
                        remaining.components.seconds * 1000
                            + remaining.components.attoseconds / 1_000_000_000_000_000
                    )
                )
                if remainingMs <= 0 { break }

                let waitMs = min(remainingMs, 100)
                for sock in sockets {
                    // Reset context for this packet
                    context.currentPartials.removeAll()
                    context.hostnameToInstance.removeAll()

                    let recvCount = cmdns_query_recv_wait(
                        sock,
                        &recvBuf,
                        recvBuf.count,
                        recordCallback,
                        ctxPtr,
                        0,  // accept any query ID
                        waitMs
                    )

                    guard recvCount > 0 else { continue }

                    // Assemble complete service entries from accumulated records
                    for partial in context.currentPartials.values {
                        guard let hostname = partial.hostname,
                            let port = partial.port
                        else {
                            continue
                        }

                        let key = "\(partial.name).\(hostname)"
                        guard !seen.contains(key) else { continue }
                        seen.insert(key)

                        let entry = MdnsServiceEntry(
                            name: partial.name,
                            hostname: hostname,
                            port: port,
                            addresses: partial.addresses,
                            text: partial.text
                        )

                        logger.info(
                            "Discovered mDNS service",
                            metadata: [
                                "name": "\(entry.name)",
                                "hostname": "\(entry.hostname)",
                                "port": "\(entry.port)",
                                "addresses": "\(entry.addresses)",
                            ]
                        )

                        continuation.yield(entry)
                    }
                }
            }
        }

        // MARK: - Interface enumeration

        private static func openMulticastSockets(logger: Logger) -> [Int32] {
            var sockets: [Int32] = []
            var ifaddrPtr: UnsafeMutablePointer<ifaddrs>?

            guard getifaddrs(&ifaddrPtr) == 0, let firstAddr = ifaddrPtr else {
                return sockets
            }
            defer { freeifaddrs(firstAddr) }

            var current: UnsafeMutablePointer<ifaddrs>? = firstAddr
            while let ifa = current {
                defer { current = ifa.pointee.ifa_next }

                let flags = Int32(ifa.pointee.ifa_flags)
                // Skip down, loopback, non-multicast interfaces
                guard flags & IFF_UP != 0,
                    flags & IFF_LOOPBACK == 0,
                    flags & IFF_MULTICAST != 0,
                    let sa = ifa.pointee.ifa_addr
                else { continue }

                let name = String(cString: ifa.pointee.ifa_name)
                // Skip docker/virtual interfaces
                if name.hasPrefix("docker") || name.hasPrefix("br-")
                    || name.hasPrefix("veth")
                {
                    continue
                }

                if sa.pointee.sa_family == UInt16(AF_INET) {
                    var addr = sockaddr_in()
                    memcpy(&addr, sa, MemoryLayout<sockaddr_in>.size)
                    // Bind to mDNS port so we receive multicast responses
                    addr.sin_port = UInt16(5353).bigEndian
                    // Use interface-aware open to ensure multicast goes out the right interface
                    let sock = name.withCString { ifname in
                        withUnsafePointer(to: &addr) { ptr in
                            cmdns_socket_open_ipv4_iface(ptr, ifname)
                        }
                    }
                    if sock >= 0 {
                        logger.debug(
                            "Opened IPv4 mDNS socket",
                            metadata: ["interface": "\(name)", "fd": "\(sock)"]
                        )
                        sockets.append(sock)
                    } else {
                        logger.warning(
                            "Failed to open IPv4 socket",
                            metadata: ["interface": "\(name)"]
                        )
                    }
                } else if sa.pointee.sa_family == UInt16(AF_INET6) {
                    var addr = sockaddr_in6()
                    memcpy(&addr, sa, MemoryLayout<sockaddr_in6>.size)
                    // Bind to mDNS port so we receive multicast responses
                    addr.sin6_port = UInt16(5353).bigEndian
                    let sock = withUnsafePointer(to: &addr) { ptr in
                        cmdns_socket_open_ipv6(ptr)
                    }
                    if sock >= 0 {
                        logger.debug(
                            "Opened IPv6 mDNS socket",
                            metadata: ["interface": "\(name)", "fd": "\(sock)"]
                        )
                        sockets.append(sock)
                    } else {
                        logger.warning(
                            "Failed to open IPv6 socket",
                            metadata: ["interface": "\(name)"]
                        )
                    }
                }
            }

            return sockets
        }
    }

    // MARK: - Browse context & callback

    /// Accumulates records from a single response packet.
    private final class PartialService {
        var name: String
        var hostname: String?
        var port: UInt16?
        var addresses: [String] = []
        var text: [String: String] = [:]

        init(name: String) {
            self.name = name
        }
    }

    /// Context passed through the C callback via user_data pointer.
    private final class BrowseContext {
        let serviceType: String
        /// Partials accumulated during one cmdns_query_recv call, keyed by instance FQDN.
        var currentPartials: [String: PartialService] = [:]
        /// Map from hostname to instance FQDNs, for matching A/AAAA records to partials.
        var hostnameToInstance: [String: String] = [:]

        init(serviceType: String) {
            self.serviceType = serviceType
        }
    }

    /// Extract a Swift String from a cmdns_string_t (non-null-terminated).
    private func extractString(_ s: cmdns_string_t) -> String? {
        guard let ptr = s.str, s.length > 0 else { return nil }
        return ptr.withMemoryRebound(to: UInt8.self, capacity: s.length) { ubuf in
            String(bytes: UnsafeBufferPointer(start: ubuf, count: s.length), encoding: .utf8)
        }
    }

    /// The C callback invoked by cmdns_query_recv for each DNS record.
    private let recordCallback: cmdns_callback_fn = {
        sock,
        from,
        addrlen,
        entryType,
        queryId,
        rtype,
        rclass,
        ttl,
        data,
        size,
        nameOffset,
        nameLength,
        recordOffset,
        recordLength,
        userData
            -> Int32 in

        guard let userData else { return 0 }
        let ctx = Unmanaged<BrowseContext>.fromOpaque(userData).takeUnretainedValue()

        var nameBuf = [CChar](repeating: 0, count: 256)

        switch Int32(rtype) {
        case Int32(CMDNS_RECORDTYPE_PTR):
            // PTR record data = the instance FQDN (e.g., "mydevice._wendyos._udp.local.")
            let ptr = cmdns_record_parse_ptr(
                data,
                size,
                recordOffset,
                recordLength,
                &nameBuf,
                nameBuf.count
            )
            guard let fqdn = extractString(ptr) else { return 0 }

            // Extract short name: everything before ".<serviceType>"
            let shortName: String
            // Remove trailing dot for comparison
            let svcType =
                ctx.serviceType.hasSuffix(".")
                ? String(ctx.serviceType.dropLast()) : ctx.serviceType
            let fqdnClean = fqdn.hasSuffix(".") ? String(fqdn.dropLast()) : fqdn

            if let range = fqdnClean.range(of: ".\(svcType)", options: .caseInsensitive) {
                shortName = String(fqdnClean[..<range.lowerBound])
            } else {
                shortName = fqdnClean
            }

            if ctx.currentPartials[fqdnClean] == nil {
                ctx.currentPartials[fqdnClean] = PartialService(name: shortName)
            }

        case Int32(CMDNS_RECORDTYPE_SRV):
            // SRV record: extract the record name to find which instance it belongs to
            var nameOfs = nameOffset
            let recordName = cmdns_string_extract(
                data,
                size,
                &nameOfs,
                &nameBuf,
                nameBuf.count
            )
            guard let instanceFQDN = extractString(recordName) else { return 0 }

            var srvBuf = [CChar](repeating: 0, count: 256)
            let srv = cmdns_record_parse_srv(
                data,
                size,
                recordOffset,
                recordLength,
                &srvBuf,
                srvBuf.count
            )
            guard let hostname = extractString(srv.name) else { return 0 }

            let key =
                instanceFQDN.hasSuffix(".") ? String(instanceFQDN.dropLast()) : instanceFQDN
            if let partial = ctx.currentPartials[key] {
                partial.hostname = hostname.hasSuffix(".") ? String(hostname.dropLast()) : hostname
                partial.port = srv.port
                ctx.hostnameToInstance[partial.hostname!] = key
            }

        case Int32(CMDNS_RECORDTYPE_TXT):
            // TXT record: key=value pairs
            var nameOfs = nameOffset
            let recordName = cmdns_string_extract(
                data,
                size,
                &nameOfs,
                &nameBuf,
                nameBuf.count
            )
            guard let instanceFQDN = extractString(recordName) else { return 0 }

            var txtRecords = [cmdns_txt_t](repeating: cmdns_txt_t(), count: 16)
            let count = cmdns_record_parse_txt(
                data,
                size,
                recordOffset,
                recordLength,
                &txtRecords,
                16
            )

            let key =
                instanceFQDN.hasSuffix(".") ? String(instanceFQDN.dropLast()) : instanceFQDN
            if let partial = ctx.currentPartials[key] {
                for i in 0..<count {
                    if let k = extractString(txtRecords[i].key) {
                        let v = extractString(txtRecords[i].value) ?? ""
                        partial.text[k] = v
                    }
                }
            }

        case Int32(CMDNS_RECORDTYPE_A):
            // A record: the record name is the hostname, not the instance
            var nameOfs = nameOffset
            let recordName = cmdns_string_extract(
                data,
                size,
                &nameOfs,
                &nameBuf,
                nameBuf.count
            )
            guard let hostname = extractString(recordName) else { return 0 }
            let cleanHost = hostname.hasSuffix(".") ? String(hostname.dropLast()) : hostname

            var addr = sockaddr_in()
            _ = cmdns_record_parse_a(data, size, recordOffset, recordLength, &addr)

            var addrStr = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
            if inet_ntop(AF_INET, &addr.sin_addr, &addrStr, socklen_t(INET_ADDRSTRLEN)) != nil {
                let ip = String(cString: addrStr)
                if let instanceKey = ctx.hostnameToInstance[cleanHost],
                    let partial = ctx.currentPartials[instanceKey]
                {
                    partial.addresses.append(ip)
                }
            }

        case Int32(CMDNS_RECORDTYPE_AAAA):
            var nameOfs = nameOffset
            let recordName = cmdns_string_extract(
                data,
                size,
                &nameOfs,
                &nameBuf,
                nameBuf.count
            )
            guard let hostname = extractString(recordName) else { return 0 }
            let cleanHost = hostname.hasSuffix(".") ? String(hostname.dropLast()) : hostname

            var addr = sockaddr_in6()
            _ = cmdns_record_parse_aaaa(data, size, recordOffset, recordLength, &addr)

            var addrStr = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
            if inet_ntop(AF_INET6, &addr.sin6_addr, &addrStr, socklen_t(INET6_ADDRSTRLEN)) != nil {
                let ip = String(cString: addrStr)
                if let instanceKey = ctx.hostnameToInstance[cleanHost],
                    let partial = ctx.currentPartials[instanceKey]
                {
                    partial.addresses.append(ip)
                }
            }

        default:
            break
        }

        return 0
    }
#endif
