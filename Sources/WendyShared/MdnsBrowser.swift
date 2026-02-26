#if os(Linux)
    import DNSClient
    import Foundation
    import Logging
    import NIOCore
    import NIOPosix

    /// A discovered mDNS service entry.
    package struct MdnsServiceEntry: Sendable {
        package let name: String
        package let hostname: String
        package let port: UInt16
        package let addresses: [String]
        package let text: [String: String]
    }

    /// Browse for mDNS services on the local network using DNSClient multicast queries.
    package enum MdnsBrowser {

        /// Resolve a `.local` hostname to IP addresses using mDNS A/AAAA queries.
        /// Returns addresses with scope IDs for link-local IPv6 (e.g., `fe80::1%eth0`).
        /// Prefers non-link-local IPv4 addresses.
        ///
        /// Per RFC 6762 §20, queries are sent on both IPv4 (224.0.0.251) and IPv6
        /// (FF02::FB) multicast groups to reach both IPv4-only and IPv6-only responders.
        package static func resolveHostname(
            _ hostname: String,
            timeout: Duration = .seconds(2),
            logger: Logger
        ) async -> [String] {
            let devices = multicastDevices()
            guard !devices.isEmpty else { return [] }

            let group = MultiThreadedEventLoopGroup(numberOfThreads: 1)

            let queryName = hostname.hasSuffix(".") ? String(hostname.dropLast()) : hostname
            let timeAmount = durationToTimeAmount(timeout)

            // Open one multicast client per interface
            var clients: [(MulticastDNSClient, String)] = []
            for device in devices {
                if let client = try? await DNSClient.connectMulticast(
                    on: group,
                    interface: device
                ).get() {
                    clients.append((client, device.name))
                }
            }

            guard !clients.isEmpty else {
                closeClients(clients)
                try? await group.shutdownGracefully()
                return []
            }

            // Send A and AAAA queries on all clients concurrently
            var futures: [(EventLoopFuture<[Message]>, String)] = []
            for (client, ifName) in clients {
                futures.append(
                    (
                        client.sendMulticastQuery(
                            forHost: queryName,
                            type: .a,
                            timeout: timeAmount
                        ),
                        ifName
                    )
                )
                futures.append(
                    (
                        client.sendMulticastQuery(
                            forHost: queryName,
                            type: .aaaa,
                            timeout: timeAmount
                        ),
                        ifName
                    )
                )
            }

            var addresses: [String] = []
            for (future, ifName) in futures {
                guard let messages = try? await future.get() else { continue }
                for message in messages {
                    for record in message.answers + message.additionalData {
                        switch record {
                        case .a(let rr):
                            let ip = rr.resource.stringAddress
                            if !addresses.contains(ip) {
                                addresses.append(ip)
                            }
                        case .aaaa(let rr):
                            var ip = rr.resource.stringAddress
                            if ip.hasPrefix("fe80:") {
                                ip = "\(ip)%\(ifName)"
                            }
                            if !addresses.contains(ip) {
                                addresses.append(ip)
                            }
                        default:
                            break
                        }
                    }
                }
            }

            closeClients(clients)
            try? await group.shutdownGracefully()

            logger.debug(
                "mDNS hostname resolution",
                metadata: [
                    "hostname": "\(hostname)",
                    "addresses": "\(addresses)",
                ]
            )
            return addresses
        }

        /// Browse for services of the given type, yielding entries as they are discovered.
        package static func browse(
            serviceType: String,
            timeout: Duration,
            logger: Logger
        ) -> AsyncStream<MdnsServiceEntry> {
            AsyncStream { continuation in
                let task = Task.detached {
                    await performBrowse(
                        serviceType: serviceType,
                        timeout: timeout,
                        logger: logger,
                        continuation: continuation
                    )
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
        ) async {
            let devices = multicastDevices()
            guard !devices.isEmpty else {
                logger.warning("No multicast-capable interfaces found")
                return
            }

            let group = MultiThreadedEventLoopGroup(numberOfThreads: 1)

            // Open one multicast client per interface
            var clients: [(MulticastDNSClient, String)] = []
            for device in devices {
                do {
                    let client = try await DNSClient.connectMulticast(
                        on: group,
                        interface: device
                    ).get()
                    clients.append((client, device.name))
                } catch {
                    logger.debug(
                        "Failed to create mDNS client",
                        metadata: ["interface": "\(device.name)", "error": "\(error)"]
                    )
                }
            }

            guard !clients.isEmpty else {
                logger.warning("No multicast clients opened")
                try? await group.shutdownGracefully()
                return
            }

            let timeAmount = durationToTimeAmount(timeout)

            // Normalize service type: remove trailing dot for comparison
            let svcType =
                serviceType.hasSuffix(".")
                ? String(serviceType.dropLast()) : serviceType

            // Send PTR queries on all interfaces concurrently
            var futures: [(EventLoopFuture<[Message]>, String)] = []
            for (client, ifName) in clients {
                futures.append(
                    (
                        client.sendMulticastQuery(
                            forHost: serviceType,
                            type: .ptr,
                            timeout: timeAmount
                        ),
                        ifName
                    )
                )
            }

            var seen = Set<String>()

            for (future, ifName) in futures {
                let messages: [Message]
                do {
                    messages = try await future.get()
                } catch {
                    logger.warning(
                        "Query failed",
                        metadata: ["interface": "\(ifName)", "error": "\(error)"]
                    )
                    continue
                }
                guard !messages.isEmpty else { continue }

                for message in messages {
                    let allRecords = message.answers + message.additionalData

                    // First pass: find PTR records to identify service instances
                    var instances: [String: String] = [:]  // fqdn -> short name
                    for record in allRecords {
                        if case .ptr(let rr) = record {
                            let fqdn = rr.resource.domainName.string
                            guard
                                let range = fqdn.range(
                                    of: ".\(svcType)",
                                    options: .caseInsensitive
                                )
                            else { continue }
                            let shortName = String(fqdn[..<range.lowerBound])
                            instances[fqdn] = shortName
                        }
                    }

                    guard !instances.isEmpty else { continue }

                    // Second pass: collect SRV, TXT, A, AAAA keyed by record owner name
                    var srvData: [String: (hostname: String, port: UInt16)] = [:]
                    var txtData: [String: [String: String]] = [:]
                    var addrData: [String: [String]] = [:]

                    for record in allRecords {
                        switch record {
                        case .srv(let rr):
                            let owner = rr.domainName.string
                            let host = rr.resource.domainName.string
                            srvData[owner] = (hostname: host, port: rr.resource.port)
                        case .txt(let rr):
                            let owner = rr.domainName.string
                            txtData[owner] = rr.resource.values
                        case .a(let rr):
                            let owner = rr.domainName.string
                            addrData[owner, default: []].append(rr.resource.stringAddress)
                        case .aaaa(let rr):
                            let owner = rr.domainName.string
                            var ip = rr.resource.stringAddress
                            if ip.hasPrefix("fe80:") {
                                ip = "\(ip)%\(ifName)"
                            }
                            addrData[owner, default: []].append(ip)
                        default:
                            break
                        }
                    }

                    // Assemble service entries
                    for (fqdn, shortName) in instances {
                        guard let srv = srvData[fqdn] else { continue }

                        let key = "\(shortName).\(srv.hostname)"
                        guard !seen.contains(key) else { continue }
                        seen.insert(key)

                        let addresses = addrData[srv.hostname] ?? []

                        let entry = MdnsServiceEntry(
                            name: shortName,
                            hostname: srv.hostname,
                            port: srv.port,
                            addresses: addresses,
                            text: txtData[fqdn] ?? [:]
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

            closeClients(clients)
            try? await group.shutdownGracefully()
        }

        // MARK: - Helpers

        private static func closeClients(_ clients: [(MulticastDNSClient, String)]) {
            for (client, _) in clients { _ = client.close() }
        }

        /// Enumerate multicast-capable, non-loopback network devices (both IPv4 and IPv6).
        /// Per RFC 6762 §20, dual-stack hosts should query using both address families.
        private static func multicastDevices() -> [NIONetworkDevice] {
            guard let devices = try? System.enumerateDevices() else { return [] }
            return devices.filter { device in
                guard device.multicastSupported,
                    let addr = device.address,
                    addr.protocol.rawValue == PF_INET || addr.protocol.rawValue == PF_INET6
                else { return false }

                // Skip loopback IPv6 (::1)
                if case .v6(let v6) = addr {
                    let a = v6.address.sin6_addr
                    let bytes = withUnsafeBytes(of: a) { Array($0) }
                    let isLoopback =
                        bytes.dropFirst().dropLast().allSatisfy({ $0 == 0 })
                        && bytes.first == 0 && bytes.last == 1
                    if isLoopback { return false }
                }

                let name = device.name
                // Skip docker/virtual interfaces
                if name.hasPrefix("docker") || name.hasPrefix("br-")
                    || name.hasPrefix("veth")
                {
                    return false
                }
                return true
            }
        }

        /// Convert Swift Duration to NIO TimeAmount.
        private static func durationToTimeAmount(_ duration: Duration) -> TimeAmount {
            let ns =
                duration.components.seconds * 1_000_000_000
                + duration.components.attoseconds / 1_000_000_000
            return .nanoseconds(ns)
        }
    }
#endif
