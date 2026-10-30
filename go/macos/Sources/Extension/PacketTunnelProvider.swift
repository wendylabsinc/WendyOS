import Foundation
import NetworkExtension

final class PacketTunnelProvider: NEPacketTunnelProvider {
    private let lock = NSLock()
    private var generation: UInt64 = 0
    private var running = false
    private var refreshTimer: DispatchSourceTimer?
    private let refreshQueue = DispatchQueue(label: "dev.wendy.network.refresh")
    private let outputQueue = DispatchQueue(label: "dev.wendy.desktop.packets", qos: .userInitiated)

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let data = options?["configuration"] as? Data,
              let config = try? PropertyListDecoder().decode(VPNConfiguration.self, from: data) else {
            completionHandler(failure("Open Wendy and enable the VPN again.")); return
        }
        #if os(iOS)
        let shared = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: "group.dev.wendy.mobile")?.appendingPathComponent("Wendy", isDirectory: true).path
        guard let shared, config.bootstrapDirectory == shared else {
            completionHandler(failure("VPN requires Wendy's shared app profile.")); return
        }
        #endif
        if let error = config.withCValue({ WVPNStart($0) }) {
            let message = String(cString: error)
            WVPNFree(error)
            completionHandler(failure(message)); return
        }
        guard let pointer = WVPNConfiguration() else { WVPNStop(); completionHandler(failure("Missing VPN settings.")); return }
        let actualData = Data(String(cString: pointer).utf8)
        WVPNFree(pointer)
        guard let actual = try? JSONDecoder().decode(VPNConfiguration.self, from: actualData) else {
            WVPNStop(); completionHandler(failure("Invalid VPN settings.")); return
        }
        let settings = actual.networkSettings()
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
            self.startRefresh(generation)
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        lock.lock(); running = false; generation &+= 1; refreshTimer?.cancel(); refreshTimer = nil; lock.unlock()
        WVPNStop()
        completionHandler()
    }

    private func startRefresh(_ generation: UInt64) {
        let timer = DispatchSource.makeTimerSource(queue: refreshQueue)
        timer.schedule(deadline: .now() + 5, repeating: 5)
        timer.setEventHandler { [weak self] in
            guard let self, self.isRunning(generation), let pointer = WVPNRefresh() else { return }
            let data = Data(String(cString: pointer).utf8)
            WVPNFree(pointer)
            guard self.isRunning(generation), let config = try? JSONDecoder().decode(VPNConfiguration.self, from: data) else { return }
            self.setTunnelNetworkSettings(config.networkSettings()) { [weak self] error in
                guard let self, self.isRunning(generation) else { return }
                if let error { self.cancelTunnelWithError(error) }
            }
        }
        lock.lock()
        if running && self.generation == generation { refreshTimer = timer } else { timer.cancel() }
        timer.resume()
        lock.unlock()
    }

    private func isRunning(_ expected: UInt64) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return running && generation == expected
    }

    private func readPackets(_ generation: UInt64) {
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self, self.isRunning(generation) else { return }
            for (packet, family) in zip(packets, protocols) where family.int32Value == AF_INET || family.int32Value == AF_INET6 {
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
                packetFlow.writePackets([Data(bytes: buffer, count: Int(count))], withProtocols: [NSNumber(value: buffer.load(as: UInt8.self) >> 4 == 6 ? AF_INET6 : AF_INET)])
            }
            if count < 0 { break }
            if Date() >= nextHealthCheck {
                nextHealthCheck = Date().addingTimeInterval(5)
                if WVPNHealthy() == 0 {
                    lock.lock(); running = false; lock.unlock()
                    WVPNStop()
                    cancelTunnelWithError(failure("Wendy lost its device relay. Open Wendy and reconnect."))
                    break
                }
            }
        }
    }

    private func failure(_ message: String) -> NSError {
        NSError(domain: "dev.wendy.desktop.network", code: 1, userInfo: [NSLocalizedDescriptionKey: message])
    }
}