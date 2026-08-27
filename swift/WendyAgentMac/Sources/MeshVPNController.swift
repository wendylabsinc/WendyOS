import Combine
import Foundation
@preconcurrency import NetworkExtension
import WendyAgentCore

@MainActor
final class MeshVPNController: NSObject, ObservableObject {
    enum Status: Equatable {
        case disabled, activatingExtension, needsApproval, connecting, connected
        case failed(String)
    }
    @Published var status: Status = .disabled

    private let extensionID = MeshSystemExtensionInstaller.extensionID
    private let autoConnectKey = "wendyMeshAutoConnect"
    private let profileConfigurationVersion = 6
    private var manager: NETransparentProxyManager?
    private var packetManager: NETunnelProviderManager?
    private var statusObserver: (any NSObjectProtocol)?

    override init() {
        super.init()
        Task { await refreshStatus() }
    }

    isolated deinit {
        if let statusObserver {
            NotificationCenter.default.removeObserver(statusObserver)
        }
    }

    func connectAutomatically(
        sessionSource: LiveCloudSessionSource = LiveCloudSessionSource()
    ) async {
        guard ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] == nil else {
            return
        }
        guard status == .disabled || isFailure else { return }
        guard UserDefaults.standard.bool(forKey: autoConnectKey) else { return }
        guard let session = sessionSource.load() else { return }
        await enable(session: session)
    }

    func connect(sessionSource: LiveCloudSessionSource = LiveCloudSessionSource()) async {
        guard let session = sessionSource.load() else {
            status = .failed("Sign in with `wendy auth login` before connecting Wendy Mesh.")
            return
        }
        await enable(session: session)
    }

    func enable(session: CloudDiscoverySession) async {
        UserDefaults.standard.set(true, forKey: autoConnectKey)
        do {
            status = .connecting
            let directory = try await MeshDirectorySync.refresh(session: session)

            status = .activatingExtension
            let installationResult = try await MeshSystemExtensionInstaller.shared.installOrUpdate {
                self.status = .needsApproval
            }
            guard installationResult == .ready else { return }
            status = .connecting
            let manager = try await configureManager(session: session)
            if manager.connection.status == .disconnected || manager.connection.status == .invalid {
                try manager.connection.startVPNTunnel(
                    options: try startOptions(
                        session: session,
                        directory: directory
                    )
                )
            }
            let packetManager = try await configurePacketManager(session: session)
            if packetManager.connection.status == .disconnected
                || packetManager.connection.status == .invalid
            {
                try packetManager.connection.startVPNTunnel(
                    options: try startOptions(
                        session: session,
                        directory: directory
                    )
                )
            }
            updateStatus(manager.connection.status)
        } catch {
            status = .failed(error.localizedDescription)
        }
    }

    func disable() async {
        UserDefaults.standard.set(false, forKey: autoConnectKey)
        if manager == nil {
            manager = try? await loadManager()
        }
        if packetManager == nil {
            packetManager = try? await loadPacketManager()
        }
        packetManager?.connection.stopVPNTunnel()
        packetManager?.isEnabled = false
        try? await packetManager?.saveToPreferences()
        guard let manager else {
            status = .disabled
            return
        }
        manager.connection.stopVPNTunnel()
        manager.isEnabled = false
        try? await manager.saveToPreferences()
        status = .disabled
    }

    private var isFailure: Bool {
        if case .failed = status { return true }
        return false
    }

    private func refreshStatus() async {
        do {
            let manager = try await loadManager()
            self.manager = manager
            observeStatus(of: manager)
            updateStatus(manager.connection.status)
        } catch {
            status = .disabled
        }
    }

    private func loadManager() async throws -> NETransparentProxyManager {
        try await existingManager() ?? NETransparentProxyManager()
    }

    private func existingManager() async throws -> NETransparentProxyManager? {
        let managers = try await NETransparentProxyManager.loadAllFromPreferences()
        return managers.first(where: { manager in
            (manager.protocolConfiguration as? NETunnelProviderProtocol)?
                .providerBundleIdentifier == extensionID
        })
    }

    private func loadPacketManager() async throws -> NETunnelProviderManager {
        try await existingPacketManager() ?? NETunnelProviderManager()
    }

    private func existingPacketManager() async throws -> NETunnelProviderManager? {
        let managers = try await NETunnelProviderManager.loadAllFromPreferences()
        return managers.first(where: { manager in
            (manager.protocolConfiguration as? NETunnelProviderProtocol)?
                .providerBundleIdentifier == extensionID
        })
    }

    private func configureManager(
        session: CloudDiscoverySession
    ) async throws -> NETransparentProxyManager {
        let manager = try await loadManager()
        manager.localizedDescription = "Wendy Mesh"

        let providerConfiguration: [String: Any] = [
            "configurationVersion": profileConfigurationVersion,
            "cloudGRPC": session.cloudGRPC,
        ]

        if manager.isEnabled,
            manager.connection.status == .connected,
            let currentProtocol = manager.protocolConfiguration as? NETunnelProviderProtocol,
            currentProtocol.providerBundleIdentifier == extensionID,
            let current = currentProtocol.providerConfiguration,
            current["configurationVersion"] as? Int == profileConfigurationVersion,
            current["cloudGRPC"] as? String == session.cloudGRPC
        {
            self.manager = manager
            observeStatus(of: manager)
            return manager
        }

        if manager.connection.status != .disconnected && manager.connection.status != .invalid {
            manager.connection.stopVPNTunnel()
            await waitUntilDisconnected(manager.connection)
        }

        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = extensionID
        proto.serverAddress = "Wendy Cloud"
        proto.providerConfiguration = providerConfiguration
        manager.protocolConfiguration = proto
        manager.isEnabled = true
        try await manager.saveToPreferences()
        try await manager.loadFromPreferences()
        self.manager = manager
        observeStatus(of: manager)
        return manager
    }

    private func configurePacketManager(
        session: CloudDiscoverySession
    ) async throws -> NETunnelProviderManager {
        let manager = try await loadPacketManager()
        manager.localizedDescription = "Wendy Mesh Ping"
        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = extensionID
        proto.serverAddress = "Wendy Cloud"
        proto.providerConfiguration = [
            "configurationVersion": profileConfigurationVersion,
            "cloudGRPC": session.cloudGRPC,
        ]
        manager.protocolConfiguration = proto
        manager.isEnabled = true
        try await manager.saveToPreferences()
        try await manager.loadFromPreferences()
        self.packetManager = manager
        return manager
    }

    private func startOptions(
        session: CloudDiscoverySession,
        directory: WendyMeshDirectory
    ) throws -> [String: NSObject] {
        return [
            "credentialBlob": try JSONEncoder().encode(session.credentials) as NSData,
            "deviceDirectory": try WendyMeshDirectory.encode(directory) as NSData,
        ]
    }

    private func waitUntilDisconnected(_ connection: NEVPNConnection) async {
        let deadline = Date().addingTimeInterval(8)
        while Date() < deadline,
            connection.status != .disconnected,
            connection.status != .invalid
        {
            try? await Task.sleep(for: .milliseconds(100))
        }
    }

    private func observeStatus(of manager: NETransparentProxyManager) {
        if let statusObserver {
            NotificationCenter.default.removeObserver(statusObserver)
        }
        statusObserver = NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: manager.connection,
            queue: .main
        ) { [weak self, weak manager] _ in
            guard let manager else { return }
            Task { @MainActor in self?.updateStatus(manager.connection.status) }
        }
    }

    private func updateStatus(_ vpnStatus: NEVPNStatus) {
        switch vpnStatus {
        case .connected:
            status = .connected
        case .connecting, .reasserting, .disconnecting:
            status = .connecting
        case .invalid, .disconnected:
            status = .disabled
        @unknown default:
            status = .disabled
        }
    }
}
