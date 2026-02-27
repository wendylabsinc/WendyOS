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

            let group = MultiThreadedEventLoopGroup.singleton
            let queryName = hostname.hasSuffix(".") ? String(hostname.dropLast()) : hostname
            let timeAmount = durationToTimeAmount(timeout)

            let clients = await openClients(on: group, devices: devices, logger: logger)
            defer { closeClients(clients) }
            guard !clients.isEmpty else { return [] }

            // Send A and AAAA queries on all interfaces concurrently
            let addresses = await withTaskGroup(
                of: [(address: String, ifName: String)].self
            ) { taskGroup in
                for (client, ifName) in clients {
                    taskGroup.addTask {
                        await Self.queryAddresses(
                            client: client, ifName: ifName,
                            queryName: queryName, timeout: timeAmount
                        )
                    }
                }

                var result: [String] = []
                for await batch in taskGroup {
                    for (ip, _) in batch where !result.contains(ip) {
                        result.append(ip)
                    }
                }
                return result
            }

            logger.debug(
                "mDNS hostname resolution",
                metadata: [
                    "hostname": "\(hostname)",
                    "addresses": "\(addresses)",
                ]
            )
            return addresses
        }

        /// Browse for services of the given type, calling `onEntry` for each discovered service.
        package static func browse(
            serviceType: String,
            timeout: Duration,
            logger: Logger,
            onEntry: (MdnsServiceEntry) -> Void
        ) async {
            let devices = multicastDevices()
            guard !devices.isEmpty else {
                logger.warning("No multicast-capable interfaces found")
                return
            }

            let group = MultiThreadedEventLoopGroup.singleton
            let clients = await openClients(on: group, devices: devices, logger: logger)
            defer { closeClients(clients) }

            guard !clients.isEmpty else {
                logger.warning("No multicast clients opened")
                return
            }

            let timeAmount = durationToTimeAmount(timeout)
            let svcType =
                serviceType.hasSuffix(".")
                ? String(serviceType.dropLast()) : serviceType

            // Query all interfaces concurrently
            await withTaskGroup(of: [MdnsServiceEntry].self) { taskGroup in
                for (client, ifName) in clients {
                    taskGroup.addTask {
                        await Self.queryServices(
                            client: client, ifName: ifName,
                            serviceType: serviceType, svcType: svcType,
                            timeout: timeAmount, logger: logger
                        )
                    }
                }

                var seen = Set<String>()
                for await entries in taskGroup {
                    for entry in entries {
                        let key = "\(entry.name).\(entry.hostname)"
                        guard !seen.contains(key) else { continue }
                        seen.insert(key)
                        onEntry(entry)
                    }
                }
            }
        }

        // MARK: - Query Helpers

        private static func queryAddresses(
            client: MulticastDNSClient,
            ifName: String,
            queryName: String,
            timeout: TimeAmount
        ) async -> [(address: String, ifName: String)] {
            var addresses: [(address: String, ifName: String)] = []

            for type in [DNSResourceType.a, DNSResourceType.aaaa] {
                guard
                    let messages = try? await client.sendMulticastQuery(
                        forHost: queryName,
                        type: type,
                        timeout: timeout
                    ).get()
                else { continue }

                for message in messages {
                    for record in message.answers + message.additionalData {
                        switch record {
                        case .a(let rr):
                            addresses.append((rr.resource.stringAddress, ifName))
                        case .aaaa(let rr):
                            var ip = rr.resource.stringAddress
                            if ip.hasPrefix("fe80:") {
                                ip = "\(ip)%\(ifName)"
                            }
                            addresses.append((ip, ifName))
                        default:
                            break
                        }
                    }
                }
            }

            return addresses
        }

        private static func queryServices(
            client: MulticastDNSClient,
            ifName: String,
            serviceType: String,
            svcType: String,
            timeout: TimeAmount,
            logger: Logger
        ) async -> [MdnsServiceEntry] {
            let messages: [Message]
            do {
                messages = try await client.sendMulticastQuery(
                    forHost: serviceType,
                    type: .ptr,
                    timeout: timeout
                ).get()
            } catch {
                logger.warning(
                    "Query failed",
                    metadata: ["interface": "\(ifName)", "error": "\(error)"]
                )
                return []
            }

            var entries: [MdnsServiceEntry] = []

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

                    let entry = MdnsServiceEntry(
                        name: shortName,
                        hostname: srv.hostname,
                        port: srv.port,
                        addresses: addrData[srv.hostname] ?? [],
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

                    entries.append(entry)
                }
            }

            return entries
        }

        // MARK: - Client Lifecycle

        private static func openClients(
            on group: EventLoopGroup,
            devices: [NIONetworkDevice],
            logger: Logger
        ) async -> [(MulticastDNSClient, String)] {
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
            return clients
        }

        private static func closeClients(_ clients: [(MulticastDNSClient, String)]) {
            for (client, _) in clients { _ = client.close() }
        }

        // MARK: - Helpers

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
