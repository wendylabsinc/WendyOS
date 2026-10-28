import Foundation
import NetworkExtension

final class PacketTunnelProvider: NEPacketTunnelProvider {
    private let lock = NSLock()
    private var generation: UInt64 = 0
    private var running = false
    private let outputQueue = DispatchQueue(label: "dev.wendy.desktop.packets", qos: .userInitiated)

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let data = options?["configuration"] as? Data,
              let config = try? PropertyListDecoder().decode(VPNConfiguration.self, from: data) else {
            completionHandler(failure("Open Wendy and enable the VPN again.")); return
        }
        if let error = config.withCValue({ WVPNStart($0) }) {
            let message = String(cString: error)
            WVPNFree(error)
            completionHandler(failure(message)); return
        }
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        let ipv4 = NEIPv4Settings(addresses: ["100.64.0.54"], subnetMasks: ["255.255.255.255"])
        // /32 routes claim only selected devices, plus the split DNS resolver.
        ipv4.includedRoutes = (config.routes.map(\.address) + ["100.64.0.53"]).map {
            NEIPv4Route(destinationAddress: $0, subnetMask: "255.255.255.255")
        }
        settings.ipv4Settings = ipv4
        settings.mtu = 1400
        let dns = NEDNSSettings(servers: ["100.64.0.53"])
        dns.matchDomains = ["mesh.wendy.dev", "cloud.wendy.dev"]
        dns.matchDomainsNoSearch = true
        // The cloud dashboard shares this zone. Public names refused by our
        // device resolver must still resolve through the system DNS service.
        dns.allowFailover = true
        settings.dnsSettings = dns
        setTunnelNetworkSettings(settings) { [weak self] error in
            guard let self else { WVPNStop(); completionHandler(error); return }
            if let error { WVPNStop(); completionHandler(error); return }
            self.lock.lock()
            self.generation &+= 1
            let generation = self.generation
            self.running = true
            self.lock.unlock()
            self.readPackets(generation)
            self.outputQueue.async { self.writePackets(generation) }
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        lock.lock(); running = false; generation &+= 1; lock.unlock()
        WVPNStop()
        completionHandler()
    }

    private func isRunning(_ expected: UInt64) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return running && generation == expected
    }

    private func readPackets(_ generation: UInt64) {
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self, self.isRunning(generation) else { return }
            for (packet, family) in zip(packets, protocols) where family.int32Value == AF_INET {
                packet.withUnsafeBytes { bytes in
                    guard let base = bytes.baseAddress else { return }
                    WVPNWrite(UnsafeMutableRawPointer(mutating: base), Int32(bytes.count))
                }
            }
            self.readPackets(generation)
        }
    }

    private func writePackets(_ generation: UInt64) {
        let buffer = UnsafeMutableRawPointer.allocate(byteCount: 65535, alignment: 8)
        defer { buffer.deallocate() }
        var nextHealthCheck = Date()
        while isRunning(generation) {
            let count = WVPNRead(buffer, 65535)
            if count > 0 && isRunning(generation) {
                packetFlow.writePackets([Data(bytes: buffer, count: Int(count))], withProtocols: [NSNumber(value: AF_INET)])
            }
            if count < 0 { break }
            if Date() >= nextHealthCheck {
                nextHealthCheck = Date().addingTimeInterval(5)
                if WVPNHealthy() == 0 {
                    lock.lock(); running = false; lock.unlock()
                    WVPNStop()
                    cancelTunnelWithError(failure("Wendy closed or its device relay stopped. Open Wendy and reconnect."))
                    break
                }
            }
        }
    }

    private func failure(_ message: String) -> NSError {
        NSError(domain: "dev.wendy.desktop.network", code: 1, userInfo: [NSLocalizedDescriptionKey: message])
    }
}