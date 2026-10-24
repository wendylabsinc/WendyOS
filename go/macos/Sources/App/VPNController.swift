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
    @Published var routes: [VPNRoute] = []
    @Published var setupIssue: String?
    private var manager: NETunnelProviderManager?
    private var observer: NSObjectProtocol?
    private var activation: CheckedContinuation<Void, Error>?

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
        observer = NotificationCenter.default.addObserver(forName: .NEVPNStatusDidChange, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.refreshStatus() }
        }
        Task {
            do {
                manager = try await Self.loadManager()
                refreshStatus()
                // A previous process's ephemeral relay credential is never reused.
                if manager?.connection.status == .connected { try await stop() }
            } catch { status = "VPN requires a signed build" }
        }
    }

    func start(_ configuration: VPNConfiguration) async throws {
        if let setupIssue { throw WendyError(message: setupIssue) }
        guard !changing else { throw WendyError(message: "A VPN change is already in progress.") }
        changing = true
        enabled = true
        defer { changing = false }
        status = "Installing network extension"
        do {
            try await withCheckedThrowingContinuation { continuation in
                activation = continuation
                let request = OSSystemExtensionRequest.activationRequest(forExtensionWithIdentifier: Self.extensionID, queue: .main)
                request.delegate = self
                OSSystemExtensionManager.shared.submitRequest(request)
            }
            let manager = try await Self.loadManager() ?? NETunnelProviderManager()
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = Self.extensionID
            proto.serverAddress = "Wendy mesh and cloud"
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
            try manager.connection.startVPNTunnel(options: ["configuration": config as NSData])
            routes = configuration.routes
            refreshStatus()
        } catch { enabled = false; status = "Could not start"; throw error }
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
        active = manager?.connection.status == .connected
        switch manager?.connection.status {
        case .connected, .connecting, .reasserting: enabled = true
        default: enabled = changing
        }
        switch manager?.connection.status {
        case .connected: status = "Connected"
        case .connecting: status = "Connecting"
        case .reasserting: status = "Reconnecting"
        case .disconnecting: status = "Disconnecting"
        default: if activation == nil { status = "Off" }
        }
    }

    nonisolated func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        Task { @MainActor in
            self.status = "Waiting for macOS approval"
            self.setupIssue = "Allow Wendy in System Settings > General > Login Items & Extensions > Network Extensions."
        }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
        Task { @MainActor in self.activation?.resume(throwing: error); self.activation = nil }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
        Task { @MainActor in
            if result == .completed { self.setupIssue = nil; self.activation?.resume() }
            else { self.activation?.resume(throwing: WendyError(message: "Restart your Mac to finish installing the network extension.")) }
            self.activation = nil
        }
    }
    nonisolated func request(_ request: OSSystemExtensionRequest, actionForReplacingExtension existing: OSSystemExtensionProperties, withExtension ext: OSSystemExtensionProperties) -> OSSystemExtensionRequest.ReplacementAction { .replace }
}

#endif
