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

    /// A socket opened on a specific network interface.
    private struct InterfaceSocket {
        let fd: Int32
        let interfaceName: String
    }

    /// Browse for mDNS services on the local network using the C mdns library.
    package enum MdnsBrowser {

        /// Resolve a `.local` hostname to IP addresses using mDNS A/AAAA queries.
        /// Returns addresses with scope IDs for link-local IPv6 (e.g., `fe80::1%eth0`).
        /// Prefers non-link-local IPv4 addresses.
        package static func resolveHostname(
            _ hostname: String,
            timeout: Duration = .seconds(2),
            logger: Logger
        ) async -> [String] {
            await withCheckedContinuation { continuation in
                Task.detached {
                    let result = performResolve(
                        hostname: hostname,
                        timeout: timeout,
                        logger: logger
                    )
                    continuation.resume(returning: result)
                }
            }
        }

        private static func performResolve(
            hostname: String,
            timeout: Duration,
            logger: Logger
        ) -> [String] {
            let sockets = openMulticastSockets(logger: logger)
            defer {
                for s in sockets { cmdns_socket_close(s.fd) }
            }
            guard !sockets.isEmpty else { return [] }

            // Ensure hostname has trailing dot for mDNS wire format
            let queryName = hostname.hasSuffix(".") ? hostname : hostname + "."

            // Send A and AAAA queries on all sockets
            var sendBuf = [UInt8](repeating: 0, count: 2048)
            for s in sockets {
                for recordType in [CMDNS_RECORDTYPE_A, CMDNS_RECORDTYPE_AAAA] {
                    queryName.withCString { cstr in
                        _ = cmdns_query_send(
                            s.fd,
                            UInt16(recordType),
                            cstr,
                            queryName.utf8.count,
                            &sendBuf,
                            sendBuf.count,
                            0
                        )
                    }
                }
            }

            // Collect addresses from responses
            let context = ResolveContext()
            let ctxPtr = Unmanaged.passUnretained(context).toOpaque()
            let deadline = ContinuousClock.now + timeout
            var recvBuf = [UInt8](repeating: 0, count: 4096)
            let fds = sockets.map(\.fd)
            var readyIndices = [Int32](repeating: 0, count: sockets.count)

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

                let waitMs = min(remainingMs, 200)
                let readyCount = fds.withUnsafeBufferPointer { socksBuf in
                    readyIndices.withUnsafeMutableBufferPointer { readyBuf in
                        cmdns_select_multi(
                            socksBuf.baseAddress,
                            Int32(fds.count),
                            readyBuf.baseAddress,
                            waitMs
                        )
                    }
                }
                guard readyCount > 0 else { continue }

                for r in 0..<Int(readyCount) {
                    let idx = Int(readyIndices[r])
                    let s = sockets[idx]
                    context.currentInterface = s.interfaceName

                    let recvCount = cmdns_query_recv(
                        s.fd,
                        &recvBuf,
                        recvBuf.count,
                        resolveCallback,
                        ctxPtr,
                        0
                    )
                    guard recvCount > 0 else { continue }
                }

                // If we have at least one non-link-local IPv4, we can stop early
                if context.addresses.contains(where: {
                    $0.contains(".") && !$0.hasPrefix("169.254.")
                }) {
                    break
                }
            }

            logger.debug(
                "mDNS hostname resolution",
                metadata: [
                    "hostname": "\(hostname)",
                    "addresses": "\(context.addresses)",
                ]
            )
            return context.addresses
        }

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
                for s in sockets {
                    cmdns_socket_close(s.fd)
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
            for s in sockets {
                let result = serviceType.withCString { cstr in
                    cmdns_query_send(
                        s.fd,
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
                        metadata: ["socket": "\(s.fd)"]
                    )
                }
            }

            // 3. Recv loop — single select() across all sockets per iteration
            let deadline = ContinuousClock.now + timeout
            var recvBuf = [UInt8](repeating: 0, count: 4096)
            var seen = Set<String>()

            // Context for the C callback
            let context = BrowseContext(serviceType: serviceType)
            let ctxPtr = Unmanaged.passUnretained(context).toOpaque()

            let fds = sockets.map(\.fd)
            var readyIndices = [Int32](repeating: 0, count: sockets.count)

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

                let waitMs = min(remainingMs, 200)

                // Wait for data on any socket (single select call)
                let readyCount = fds.withUnsafeBufferPointer { socksBuf in
                    readyIndices.withUnsafeMutableBufferPointer { readyBuf in
                        cmdns_select_multi(
                            socksBuf.baseAddress,
                            Int32(fds.count),
                            readyBuf.baseAddress,
                            waitMs
                        )
                    }
                }

                guard readyCount > 0 else { continue }

                // Read from each ready socket
                for r in 0..<Int(readyCount) {
                    let sock = sockets[Int(readyIndices[r])].fd

                    // Reset context for this packet
                    context.currentPartials.removeAll()
                    context.hostnameToInstance.removeAll()

                    let recvCount = cmdns_query_recv(
                        sock,
                        &recvBuf,
                        recvBuf.count,
                        recordCallback,
                        ctxPtr,
                        0  // accept any query ID
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

                        logger.debug(
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

        private static func openMulticastSockets(logger: Logger) -> [InterfaceSocket] {
            var sockets: [InterfaceSocket] = []
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
                        sockets.append(InterfaceSocket(fd: sock, interfaceName: name))
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
                        sockets.append(InterfaceSocket(fd: sock, interfaceName: name))
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
    // These context classes are only accessed from a single Task.detached and the synchronous
    // C callbacks within it — they never cross concurrency domains. Marked @unchecked Sendable
    // because they're passed through C void* user_data pointers via Unmanaged.

    private final class PartialService: @unchecked Sendable {
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
    private final class BrowseContext: @unchecked Sendable {
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

            // Only accept PTR records that match our queried service type
            guard let range = fqdnClean.range(of: ".\(svcType)", options: .caseInsensitive)
            else { return 0 }
            shortName = String(fqdnClean[..<range.lowerBound])

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

    // MARK: - Hostname resolution context & callback

    /// Context for hostname resolution (A/AAAA queries only).
    private final class ResolveContext: @unchecked Sendable {
        var addresses: [String] = []
        /// Set before each cmdns_query_recv call to the interface name of the receiving socket.
        var currentInterface: String = ""
    }

    /// C callback for hostname resolution — only handles A and AAAA records.
    private let resolveCallback: cmdns_callback_fn = {
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
        let ctx = Unmanaged<ResolveContext>.fromOpaque(userData).takeUnretainedValue()

        switch Int32(rtype) {
        case Int32(CMDNS_RECORDTYPE_A):
            var addr = sockaddr_in()
            _ = cmdns_record_parse_a(data, size, recordOffset, recordLength, &addr)

            var buf = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
            if inet_ntop(AF_INET, &addr.sin_addr, &buf, socklen_t(INET_ADDRSTRLEN)) != nil {
                let ip = String(cString: buf)
                if !ctx.addresses.contains(ip) {
                    ctx.addresses.append(ip)
                }
            }

        case Int32(CMDNS_RECORDTYPE_AAAA):
            var addr = sockaddr_in6()
            _ = cmdns_record_parse_aaaa(data, size, recordOffset, recordLength, &addr)

            var buf = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
            if inet_ntop(AF_INET6, &addr.sin6_addr, &buf, socklen_t(INET6_ADDRSTRLEN)) != nil {
                var ip = String(cString: buf)
                // Append scope ID for link-local so connect() knows which interface to use
                if ip.hasPrefix("fe80:") {
                    ip = "\(ip)%\(ctx.currentInterface)"
                }
                if !ctx.addresses.contains(ip) {
                    ctx.addresses.append(ip)
                }
            }

        default:
            break
        }

        return 0
    }
#endif
