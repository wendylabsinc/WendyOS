#if os(macOS)
import Foundation
import NetworkExtension
import SystemExtensions
import Security

@MainActor
final class VPNController: NSObject, ObservableObject, OSSystemExtensionRequestDelegate {
    static let extensionID = "dev.wendy.desktop.network"
    @Published var status = "Off"
    @Published var active = false
    @Published var enabled = false
    @Published var changing = false
    @Published var connectionError: String?
    @Published var routes: [VPNRoute] = []
    @Published var setupIssue: String?
    private var manager: NETunnelProviderManager?
    private var activation: AsyncCallback<Void>?
    private var activationRequest: OSSystemExtensionRequest?

    override init() {
        super.init()
        var code: SecCode?
        var info: CFDictionary?
        var staticCode: SecStaticCode?
        if SecCodeCopySelf([], &code) == errSecSuccess, let code,
           SecCodeCopyStaticCode(code, [], &staticCode) == errSecSuccess, let staticCode,
           SecCodeCopySigningInformation(staticCode, SecCSFlags(rawValue: kSecCSSigningInformation), &info) == errSecSuccess,
           let values = info as? [String: Any], values[kSecCodeInfoTeamIdentifier as String] as? String != nil {
            setupIssue = nil
        } else {
            setupIssue = "VPN needs a signed build with Network Extension provisioning. This development build cannot activate the VPN."
        }
    }

    /// The root view owns observation, including removal of the notification observer.
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
            // A previous process's ephemeral relay credential is never reused.
            if manager?.connection.status == .connected { try await stop() }
        } catch {
            if Task.isCancelled { return }
            connectionError = error.localizedDescription
        }
        for await change in changes.stream {
            if change == .NEVPNConfigurationChange && !changing {
                do { manager = try await Self.loadManager() }
                catch { connectionError = error.localizedDescription }
            }
            refreshStatus()
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
        if let setupIssue { throw WendyError(message: setupIssue) }
        guard !changing else { throw WendyError(message: "A VPN change is already in progress.") }
        changing = true
        connectionError = nil
        defer { changing = false; refreshStatus() }
        status = "Installing network extension"
        do {
            try Task.checkCancellation()
            let completion = AsyncCallback<Void>()
            let request = OSSystemExtensionRequest.activationRequest(forExtensionWithIdentifier: Self.extensionID, queue: .main)
            activation = completion
            activationRequest = request
            defer { activation = nil; activationRequest = nil }
            request.delegate = self
            OSSystemExtensionManager.shared.submitRequest(request)
            try await completion.value()
            try Task.checkCancellation()
            let manager = try await Self.loadManager() ?? NETunnelProviderManager()
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = Self.extensionID
            proto.serverAddress = "127.0.0.1"
            proto.disconnectOnSleep = true
            // Only non-secret metadata is saved to the system VPN profile.
            manager.protocolConfiguration = proto
            manager.localizedDescription = "Wendy devices"
            manager.isEnabled = true
            manager.isOnDemandEnabled = false
            try await manager.saveToPreferences()
            try await manager.loadFromPreferences()
            self.manager = manager
            let encoder = PropertyListEncoder()
            encoder.outputFormat = .binary
            let config = try encoder.encode(configuration)
            try Task.checkCancellation()
            try await VPNConnectionLifecycle.start(manager.connection, options: ["configuration": config as NSData])
            routes = configuration.routes
            refreshStatus()
        } catch { connectionError = error.localizedDescription; throw error }
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

    // Requests are submitted on DispatchQueue.main, so delegate callbacks are
    // synchronous main-actor boundaries and cannot race a replacement request.
    nonisolated func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        let requestID = ObjectIdentifier(request)
        MainActor.assumeIsolated {
            guard activationRequest.map(ObjectIdentifier.init) == requestID else { return }
            status = "Waiting for macOS approval"
            setupIssue = "Allow Wendy in System Settings > General > Login Items & Extensions > Network Extensions."
        }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
        let requestID = ObjectIdentifier(request)
        MainActor.assumeIsolated {
            guard activationRequest.map(ObjectIdentifier.init) == requestID else { return }
            activation?.finish(.failure(error))
        }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
        let requestID = ObjectIdentifier(request)
        MainActor.assumeIsolated {
            guard activationRequest.map(ObjectIdentifier.init) == requestID else { return }
            if result == .completed { setupIssue = nil; activation?.finish(.success(())) }
            else { activation?.finish(.failure(WendyError(message: "Restart your Mac to finish installing the network extension."))) }
        }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, actionForReplacingExtension existing: OSSystemExtensionProperties, withExtension ext: OSSystemExtensionProperties) -> OSSystemExtensionRequest.ReplacementAction { .replace }
}

#endif
