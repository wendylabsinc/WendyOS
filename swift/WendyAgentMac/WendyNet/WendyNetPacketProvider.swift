import Foundation
@preconcurrency import NetworkExtension
import WendyAgentCore
import os

private let packetLog = Logger(
    subsystem: "sh.wendy.WendyAgentMac.WendyNet",
    category: "PacketProvider"
)

/// Packet tunnel that claims the mesh route for ICMP and installs the scoped
/// resolver for Wendy mesh names. DNS queries target a VIP inside 10.99/16 and
/// are intercepted at the socket layer by WendyNetProxyProvider along with
/// other TCP/UDP flows; in practice only ICMP and stray non-flow traffic arrive
/// through packetFlow.
final class WendyNetPacketProvider: NEPacketTunnelProvider, @unchecked Sendable {
    private static let dnsServerAddress = "10.99.255.253"

    private var extensionConfig: ExtensionConfig?
    private let datagramSessions = DatagramSessionCache()

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping ((any Error)?) -> Void
    ) {
        packetLog.notice("starting packet tunnel")
        guard
            let config = ExtensionConfig.load(
                providerConfiguration: (protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerConfiguration,
                options: options
            )
        else {
            completionHandler(
                NSError(
                    domain: "sh.wendy.WendyAgentMac.WendyNet",
                    code: 2,
                    userInfo: [NSLocalizedDescriptionKey: "WendyNet configuration is unavailable"]
                )
            )
            return
        }
        extensionConfig = config

        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        // utun address: reserved top-of-range host; asset 65534 would collide,
        // which is accepted as negligible (documented in the design spec).
        let ipv4 = NEIPv4Settings(addresses: ["10.99.255.254"], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = [
            NEIPv4Route(destinationAddress: "10.99.0.0", subnetMask: "255.255.0.0")
        ]
        settings.ipv4Settings = ipv4

        let dns = NEDNSSettings(servers: [Self.dnsServerAddress])
        dns.matchDomains = ["mesh.wendy.internal", "cloud.wendy.dev"]
        dns.matchDomainsNoSearch = true
        settings.dnsSettings = dns
        settings.mtu = 1400

        setTunnelNetworkSettings(settings) { [weak self] error in
            if let error {
                packetLog.error(
                    "failed to apply tunnel settings: \(error.localizedDescription, privacy: .public)"
                )
            } else {
                packetLog.notice("packet tunnel active")
                self?.readLoop()
            }
            completionHandler(error)
        }
    }

    override func stopTunnel(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        packetLog.notice("stopping packet tunnel reason=\(reason.rawValue)")
        extensionConfig = nil
        completionHandler()
    }

    private func readLoop() {
        packetFlow.readPacketObjects { [weak self] packets in
            guard let self, let config = self.extensionConfig else { return }
            for packet in packets where packet.protocolFamily == AF_INET {
                self.handle(packet.data, config: config)
            }
            self.readLoop()
        }
    }

    private func handle(_ packetData: Data, config: ExtensionConfig) {
        guard let echo = WendyICMPv4.parseEchoRequest(packetData),
            let assetID = WendyMeshAddressPlan.deviceID(for: echo.destinationAddress)
        else {
            // Drop diagnostic: everything except ICMP echo to a VIP is dropped by design.
            // Confirmed by the Step 6 spike that only proto=1 (ICMP) ever arrives here in
            // practice, since TCP/UDP to the mesh range is intercepted earlier by
            // WendyNetProxyProvider — so this now only fires (and is worth seeing) for the
            // non-ICMP stragglers.
            let proto = packetData.count > 9 ? packetData[9] : 0
            if proto != 1 {
                packetLog.debug("utun packet proto=\(proto) len=\(packetData.count)")
            }
            return
        }
        relayEcho(echo, assetID: assetID, config: config)
    }

    private func relayEcho(
        _ echo: WendyICMPv4.EchoRequest,
        assetID: Int32,
        config: ExtensionConfig
    ) {
        Task { [weak self] in
            guard let self else { return }
            do {
                let session = try await self.datagramSessions.session(for: assetID, config: config)
                await session.setEchoHandler(identifier: UInt32(echo.identifier)) {
                    [weak self] reply in
                    // Reconstruct the on-wire reply from the verbatim echo.
                    let original = WendyICMPv4.EchoRequest(
                        sourceAddress: echo.sourceAddress,
                        destinationAddress: echo.destinationAddress,
                        identifier: UInt16(reply.identifier & 0xFFFF),
                        sequence: UInt16(reply.sequence & 0xFFFF),
                        payload: reply.payload
                    )
                    let packet = WendyICMPv4.makeEchoReply(to: original, payload: reply.payload)
                    self?.packetFlow.writePacketObjects([
                        NEPacket(data: packet, protocolFamily: sa_family_t(AF_INET))
                    ])
                }
                await session.sendEcho(
                    identifier: UInt32(echo.identifier),
                    sequence: UInt32(echo.sequence),
                    payload: echo.payload
                )
            } catch {
                packetLog.error(
                    "echo relay failed asset=\(assetID): \(error.localizedDescription, privacy: .public)"
                )
            }
        }
    }
}
