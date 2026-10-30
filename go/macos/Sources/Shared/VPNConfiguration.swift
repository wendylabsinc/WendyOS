import Foundation
import NetworkExtension

struct VPNRoute: Codable, Identifiable {
    var selector: String
    var name: String
    var address: String
    var host: String
    var id: String { selector }
}
struct VPNInternetConfiguration: Codable {
    let address4: String
    let address6: String?
    let mtu: Int
}
struct VPNConfiguration: Codable {
    let endpoint: String
    let token: String
    let routes: [VPNRoute]
    var internet: VPNInternetConfiguration? = nil
    var bootstrapDirectory: String? = nil
    var gateway: String? = nil

    func networkSettings() -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        let ipv4 = NEIPv4Settings(addresses: [internet?.address4 ?? "100.64.0.54"], subnetMasks: ["255.255.255.255"])
        // /32 routes claim only selected devices, plus the split DNS resolver.
        ipv4.includedRoutes = (routes.map(\.address) + ["100.64.0.53"]).map {
            NEIPv4Route(destinationAddress: $0, subnetMask: "255.255.255.255")
        }
        if internet != nil {
            ipv4.includedRoutes = [NEIPv4Route.default()] + (ipv4.includedRoutes ?? [])
            ipv4.excludedRoutes = [NEIPv4Route(destinationAddress: "127.0.0.0", subnetMask: "255.0.0.0")]
            // Capture IPv6 even on an IPv4-only donor, so it cannot bypass the
            // selected uplink over the phone's cellular or the Mac's WAN.
            let address6 = internet?.address6.flatMap { $0.isEmpty ? nil : $0 } ?? "fd2b:677a:9d11::ffff"
            let ipv6 = NEIPv6Settings(addresses: [address6], networkPrefixLengths: [128])
            ipv6.includedRoutes = [NEIPv6Route.default()]
            ipv6.excludedRoutes = [NEIPv6Route(destinationAddress: "::1", networkPrefixLength: 128)]
            settings.ipv6Settings = ipv6
        }
        settings.ipv4Settings = ipv4
        settings.mtu = NSNumber(value: internet?.mtu ?? 1400)
        let dns = NEDNSSettings(servers: ["100.64.0.53"])
        dns.matchDomains = internet == nil ? ["mesh.wendy.dev", "cloud.wendy.dev"] : [""]
        dns.matchDomainsNoSearch = true
        // The cloud dashboard shares this zone. Public names refused by our
        // device resolver must still resolve through the system DNS service.
        dns.allowFailover = internet == nil
        settings.dnsSettings = dns
        return settings
    }

    // Borrowed C pointers remain valid until the synchronous call returns.
    func withCValue<T>(_ body: (UnsafeMutablePointer<WDVPNConfig>) throws -> T) rethrows -> T {
        let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
        var cRoutes = routes.map {
            WDRoute(selector: strings.add($0.selector), name: strings.add($0.name),
                    address: strings.add($0.address), host: strings.add($0.host))
        }
        return try cRoutes.withUnsafeMutableBufferPointer { buffer in
            var config = WDVPNConfig(endpoint: strings.add(endpoint), token: strings.add(token),
                                     routes: buffer.baseAddress, route_count: buffer.count,
                                     internet_address4: strings.add(internet?.address4 ?? ""),
                                     internet_address6: strings.add(internet?.address6 ?? ""),
                                     internet_mtu: Int32(internet?.mtu ?? 0),
                                     bootstrap_directory: strings.add(bootstrapDirectory ?? ""),
                                     gateway: strings.add(gateway ?? ""))
            return try body(&config)
        }
    }
}
final class CStringStorage {
    private var pointers: [UnsafeMutablePointer<CChar>] = []
    func add(_ value: String) -> UnsafeMutablePointer<CChar> {
        let pointer = strdup(value)!
        pointers.append(pointer)
        return pointer
    }
    deinit { pointers.forEach { free($0) } }
}
