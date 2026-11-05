import Foundation
import NetworkExtension

@MainActor
final class VPNController: ObservableObject {
    static let extensionID = "dev.wendy.mobile.network"
    @Published var status = "Off"
    @Published var active = false
    @Published var enabled = false
    @Published var changing = false
    @Published var connectionError: String?
    @Published var routes: [VPNRoute] = []
    var setupIssue: String? {
        FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: "group.dev.wendy.mobile") == nil
            ? "VPN requires a signed build with Network Extension and App Group provisioning." : nil
    }
    private var manager: NETunnelProviderManager?

    func observeStatus() async {
        let changes = AsyncStream<Notification.Name>.makeStream(bufferingPolicy: .bufferingNewest(1))
        let observers = [Notification.Name.NEVPNStatusDidChange, .NEVPNConfigurationChange].map { name in
            NotificationCenter.default.addObserver(forName: name, object: nil, queue: .main) { _ in
                changes.continuation.yield(name)
            }
        }
        defer {
            observers.forEach(NotificationCenter.default.removeObserver)
            changes.continuation.finish()
        }
        do {
            manager = try await Self.loadManager()
            try Task.checkCancellation()
            refreshStatus()
        } catch {
            if Task.isCancelled { return }
            connectionError = error.localizedDescription
        }
        for await change in changes.stream {
            if change == .NEVPNConfigurationChange && !changing {
                do { manager = try await Self.loadManager() }
                catch { connectionError = error.localizedDescription }
            }
            let wasActive = active
            refreshStatus()
            if wasActive && manager?.connection.status == .disconnected && !changing {
                try? await GoClient.shared.stopVPN()
            }
            if let connection = manager?.connection, connection.status == .disconnected {
                let error = await VPNConnectionLifecycle.disconnectError(connection)
                // A delayed error from an old session must not overwrite a new one.
                if manager?.connection === connection, connection.status == .disconnected, !changing {
                    connectionError = error
                }
            }
        }
    }

    func start(_ configuration: VPNConfiguration) async throws {
        guard !changing else { throw WendyError(message: "A VPN change is already in progress.") }
        if let setupIssue { throw WendyError(message: setupIssue) }
        changing = true
        connectionError = nil
        defer { changing = false; refreshStatus() }
        let manager = try await Self.loadManager() ?? NETunnelProviderManager()
        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = Self.extensionID
        proto.serverAddress = "127.0.0.1"
        proto.disconnectOnSleep = false
        let encoder = PropertyListEncoder()
        encoder.outputFormat = .binary
        proto.providerConfiguration = ["bootstrap": try encoder.encode(MobileVPNBootstrap(configuration))]
        manager.protocolConfiguration = proto
        manager.localizedDescription = "Wendy"
        manager.isEnabled = true
        manager.isOnDemandEnabled = false
        try await manager.saveToPreferences()
        try await manager.loadFromPreferences()
        self.manager = manager
        try Task.checkCancellation()
        try await VPNConnectionLifecycle.start(manager.connection, options: ["configuration": try encoder.encode(configuration) as NSData])
        routes = configuration.routes
        refreshStatus()
    }
    func stop() async throws {
        if manager == nil { manager = try await Self.loadManager() }
        guard let manager else { return }
        manager.connection.stopVPNTunnel()
        for _ in 0..<100 {
            if manager.connection.status == .disconnected || manager.connection.status == .invalid { break }
            try await Task.sleep(for: .milliseconds(100))
        }
        guard manager.connection.status == .disconnected || manager.connection.status == .invalid else {
            throw WendyError(message: "The VPN has not disconnected yet. Try again.")
        }
        routes = []
        refreshStatus()
    }
    private static func loadManager() async throws -> NETunnelProviderManager? {
        try await NETunnelProviderManager.loadAllFromPreferences().first {
            ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == extensionID
        }
    }
    private func refreshStatus() {
        updateStatus(manager?.connection.status ?? .invalid)
    }

    // Keep the toggle independent of requested state, provisioning, and pending
    // start/stop work. Only a connected OS tunnel can turn it on.
    func updateStatus(_ connectionStatus: NEVPNStatus) {
        let connected = connectionStatus == .connected
        if active != connected { active = connected }
        if enabled != connected { enabled = connected }
        let next: String
        switch connectionStatus {
        case .connected: next = "Connected"
        case .connecting: next = "Connecting"
        case .reasserting: next = "Reconnecting"
        case .disconnecting: next = "Disconnecting"
        default: next = "Disconnected"
        }
        if status != next { status = next }
        if connected && connectionError != nil { connectionError = nil }
    }

}
