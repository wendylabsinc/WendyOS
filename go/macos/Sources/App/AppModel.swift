#if os(macOS)
import AppKit
#else
import UIKit
#endif
import SwiftUI

@MainActor
final class AppModel: ObservableObject {
    @Published var nearby: [Device] = []
    @Published var cloud: [Device] = []
    @Published var selection: Device?
    @Published var connected: Device?
    @Published var appActions: Set<String> = []
    @Published var appDeletion: AppDeletion?
    @Published var apps: [DeviceApp] = []
    @Published var cameras: [Camera] = []
    @Published var logs: [LogEntry] = []
    @Published var cameraFeeds: [CameraFeed] = []
    @Published var logApp = ""
    @Published var connectingDevice: Device?
    @Published var failedConnection: Device?
    @Published var connectionError: String?
    private var connectionAttempt: UInt64 = 0
    private let connectDevice: (String, UInt64, Bool) async throws -> UInt64

    init(connectDevice: @escaping (String, UInt64, Bool) async throws -> UInt64 = { selector, request, forgetIdentity in
        try await GoClient.shared.connect(selector: selector, request: request, forgetIdentity: forgetIdentity)
    }) {
        self.connectDevice = connectDevice
    }

    var selectedDeviceReady: Bool {
        selection != nil && selection?.id == connected?.id && connectingDevice == nil && detailError == nil
    }
    @Published var discovering = false
    @Published var signingIn = false
    @Published var account: CloudAccount?
    @Published var accountError: String?
    @Published var initialized = false
    @Published var alert: String?
    @Published var nearbyError: String?
    @Published var cloudError: String?
    @Published var canForgetIdentity = false
    @Published var detailError: String?
    @Published var appsError: String?
    @Published var camerasError: String?
    @Published var logError: String?
    @Published var vpnUpdating = false
    @Published var vpnError: String?
    @Published var selectedRoutes: Set<String> = []
    @Published var loginURL: URL?
    #if os(iOS)
    @Published var webpageURL: URL?
    @Published var mobileCloudAccounts: [SavedAccount] = []
    @Published var mobileCloudOrganizations: [CloudOrganization] = []
    @Published var mobileCloudAccount = ""
    @Published var mobileCloudOrganization = ""
    @Published var mobileCloudSelectionLoading = false
    @Published var mobileCloudSelectionError: String?
    var mobileCloudSelectionKey: String { mobileCloudAccount + "|" + mobileCloudOrganization }
    var mobileCloudSelectionGeneration = 0
    #endif
    let client = GoClient.shared
    let vpn = VPNController()
    @Published private(set) var deviceChat = DeviceChatModel(session: 0)
    private var session: UInt64 = 0
    var deviceSession: UInt64 { session }
    private var logStream: UInt64 = 0
    private var cameraStream: UInt64 = 0
    private var cameraWallVisible = false
    private var cameraBatch: UInt64 = 0
    private var polling: Task<Void, Never>?

    func start() async {
        guard !initialized, ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] == nil else { return }
        do {
            #if os(iOS)
            try await client.initializeMobile()
            #else
            guard let cli = Bundle.main.url(forAuxiliaryExecutable: "wendy") else {
                throw WendyError(message: "The bundled Wendy CLI is missing. Rebuild the app with Scripts/build.sh.")
            }
            try await client.initialize(cliPath: cli.path)
            #endif
            initialized = true
            polling = Task { [weak self] in
                while !Task.isCancelled {
                    guard let self else { return }
                    do { for event in try await self.client.poll() { self.receive(event) } }
                    catch { self.alert = error.localizedDescription; return }
                    try? await Task.sleep(for: .milliseconds(16))
                }
            }
            await refreshAccount()
            await refreshNearby()
            if account?.signedIn == true { await refreshCloud() }
        } catch { alert = error.localizedDescription }
    }

    func refreshAccount() async {
        guard initialized else { return }
        do { account = try await client.account(); accountError = nil; await PushNotifications.shared.useAccount(account) }
        catch { accountError = error.localizedDescription }
    }

    func refreshNearby() async {
        guard initialized, !discovering else { return }
        discovering = true
        defer { discovering = false }
        do { nearby = try await client.nearby(); nearbyError = nil }
        catch { nearbyError = error.localizedDescription }
    }

    func refreshCloud() async {
        guard initialized, !discovering else { return }
        discovering = true
        defer { discovering = false }
        do { cloud = try await client.cloud(); cloudError = nil }
        catch { cloudError = error.localizedDescription }
    }

    func connect(_ device: Device, forgetIdentity: Bool = false) async {
        let attempt = client.beginConnection()
        connectionAttempt = attempt
        selection = device
        detailError = nil
        failedConnection = nil; connectionError = nil
        canForgetIdentity = false
        if connected?.id == device.id && !forgetIdentity {
            connectingDevice = nil
            return
        }
        connectingDevice = device
        do {
            let newSession = try await connectDevice(device.selector, attempt, forgetIdentity)
            guard connectionAttempt == attempt else { return }
            cameraFeeds.forEach { $0.pause() }
            cameraFeeds = []; cameras = []; apps = []; logs = []
            deviceChat.stop()
            deviceChat = DeviceChatModel(session: newSession)
            session = newSession
            selection = device; connected = device
            detailError = nil; appsError = nil; camerasError = nil; logError = nil
            connectingDevice = nil
            await refreshDetails()
        } catch {
            guard connectionAttempt == attempt else { return }
            connectingDevice = nil
            canForgetIdentity = (error as? WendyError)?.canForgetIdentity ?? false
            failedConnection = device
            detailError = error.localizedDescription
        }
    }

    func openApp(_ app: DeviceApp) async {
        guard connected != nil else { return }
        let capturedSession = session
        do {
            let url = try await client.openApp(app.appName, session: capturedSession)
            guard session == capturedSession else { return }
            #if os(macOS)
            if !NSWorkspace.shared.open(url) { alert = "Could not open the app webpage in your browser." }
            #else
            webpageURL = url
            #endif
        } catch {
            guard session == capturedSession else { return }
            alert = error.localizedDescription
        }
    }

    func setAppRunning(_ app: String, running: Bool) async {
        guard connected != nil, !appActions.contains(app) else { return }
        let capturedSession = session
        appActions.insert(app)
        defer { appActions.remove(app) }
        do {
            if running { try await client.startApp(app, session: capturedSession) }
            else { try await client.stopApp(app, session: capturedSession) }
            guard session == capturedSession else { return }
            let updated = try await client.apps()
            guard session == capturedSession else { return }
            apps = updated
        } catch {
            guard session == capturedSession else { return }
            alert = error.localizedDescription
        }
    }

    func requestAppDeletion(_ app: String) {
        guard let connected, !appActions.contains(app) else { return }
        appDeletion = AppDeletion(app: app, device: connected.title, session: session)
    }

    func stopApps(_ selected: [DeviceApp], expectedSession: UInt64) async {
        guard connected != nil, expectedSession == session else { return }
        let names = Set(selected.filter { $0.runningState != .stopped }.map(\.appName))
            .intersection(apps.map(\.appName))
        guard !names.isEmpty, appActions.isDisjoint(with: names) else { return }
        appActions.formUnion(names)
        defer { appActions.subtract(names) }
        var failures = await AppStopBatch.run(names: Array(names), isCurrent: { self.session == expectedSession }, stop: { name in
            try await self.client.stopApp(name, session: expectedSession)
        })
        guard session == expectedSession else { return }
        do {
            let updated = try await client.apps()
            guard session == expectedSession else { return }
            apps = updated
        } catch { failures.append("Refresh: \(error.localizedDescription)") }
        guard session == expectedSession else { return }
        if !failures.isEmpty { alert = "Could not complete all app stops:\n" + failures.joined(separator: "\n") }
    }

    func deleteApp(_ target: AppDeletion) async {
        guard target.session == session, connected != nil else {
            alert = "The device connection changed. Select the app again before deleting it."
            return
        }
        guard !appActions.contains(target.app) else { return }
        appActions.insert(target.app)
        defer { appActions.remove(target.app) }
        do {
            try await client.deleteApp(target.app, session: target.session)
            guard session == target.session else { return }
            apps.removeAll { $0.id == target.app }
            if logApp == target.app { try? await client.stopLogs(); logApp = "" }
            let updated = try await client.apps()
            guard session == target.session else { return }
            apps = updated
        } catch {
            guard session == target.session else { return }
            alert = error.localizedDescription
        }
    }

    func refreshDetails() async {
        guard connected != nil else { return }
        let capturedSession = session
        do {
            let result = try await client.apps()
            guard session == capturedSession else { return }
            apps = result; appsError = nil
        } catch {
            guard session == capturedSession else { return }
            appsError = error.localizedDescription
        }
        do {
            let result = try await client.cameras()
            guard session == capturedSession else { return }
            cameras = result
            let previous = Dictionary(uniqueKeysWithValues: cameraFeeds.map { ($0.camera.id, $0) })
            for feed in cameraFeeds where !cameras.contains(where: { $0.id == feed.camera.id }) {
                pauseCamera(feed)
            }
            cameraFeeds = cameras.map { previous[$0.id] ?? CameraFeed(camera: $0) }
            camerasError = nil
            if cameraWallVisible { await playCameras() }
        }
        catch { if session == capturedSession { camerasError = error.localizedDescription } }
    }

    func resources() async throws -> ResourceSnapshot { try await client.resources(session: session) }
    func ports(app: String) async throws -> [ResourcePort] { try await client.ports(app: app, session: session) }

    func streamLogs() async {
        logs = []; logError = nil
        logStream &+= 1
        let stream = logStream
        do { try await client.startLogs(app: logApp, stream: stream) }
        catch { if logStream == stream { logError = error.localizedDescription } }
    }

    func showCameraWall() {
        cameraWallVisible = true
        Task { await playCameras() }
    }

    func hideCameraWall() {
        cameraWallVisible = false
        pauseCameras()
    }

    func playCameras() async {
        cameraBatch &+= 1
        let batch = cameraBatch
        let currentSession = session
        for feed in cameraFeeds {
            guard cameraWallVisible, cameraBatch == batch, session == currentSession, !Task.isCancelled else { return }
            if !feed.running { await playCamera(feed) }
        }
    }

    func playCamera(_ feed: CameraFeed, inline: Bool = false) async {
        guard connected != nil, (cameraWallVisible || inline),
              cameraFeeds.contains(where: { $0 === feed }) else { return }
        cameraStream &+= 1
        let stream = cameraStream
        feed.start(stream: stream)
        do {
            try await client.startCamera(id: feed.camera.id, stream: stream, session: session)
        } catch {
            feed.fail(error.localizedDescription, stream: stream)
        }
    }

    func pauseCamera(_ feed: CameraFeed) {
        let stream = feed.stream
        feed.pause()
        guard stream != 0 else { return }
        let currentSession = session
        Task {
            try? await client.stopCamera(id: feed.camera.id, stream: stream, session: currentSession)
        }
    }

    func pauseCameras() {
        cameraBatch &+= 1
        for feed in cameraFeeds { pauseCamera(feed) }
    }

    func signIn(development: Bool = false) async {
        guard !signingIn else { return }
        signingIn = true
        do { try await client.login(development: development) }
        catch { signingIn = false; alert = error.localizedDescription }
    }

    func cancelLogin() async {
        do { try await client.cancelLogin(); signingIn = false; loginURL = nil }
        catch { alert = error.localizedDescription }
    }

    func enableVPN() async {
        guard !vpnUpdating else { return }
        vpnError = nil
        // Check prerequisites before stopping a working tunnel or opening Go relays.
        if let issue = vpn.setupIssue { vpnError = issue; return }
        let routes = (nearby + cloud).filter { selectedRoutes.contains($0.id) && $0.routable }
        guard !routes.isEmpty else { vpnError = "Select at least one device, then turn on VPN."; return }
        vpnUpdating = true
        defer { vpnUpdating = false }
        do {
            try await vpn.stop()
            let config = try await client.startVPN(routes: routes.map { VPNRouteSelection(selector: $0.selector, name: $0.title) })
            do { try await vpn.start(config) }
            catch { try? await client.stopVPN(); throw error }
        } catch { vpnError = error.localizedDescription }
    }

    func disableVPN() async {
        guard !vpnUpdating else { return }
        vpnError = nil
        vpnUpdating = true
        defer { vpnUpdating = false }
        do { try await vpn.stop(); try await client.stopVPN() }
        catch { vpnError = error.localizedDescription }
    }

    func shutdown() async {
        try? await vpn.stop()
        try? await client.shutdown()
        polling?.cancel()
    }

    private func receive(_ event: BridgeEvent) {
        switch event.payload {
        case .loginURL(let text):
            if let url = URL(string: text), url.scheme == "https" {
                loginURL = url
                #if os(macOS)
                NSWorkspace.shared.open(url)
                #endif
            }
        case .loginDone:
            signingIn = false; loginURL = nil
            Task { await refreshAccount(); await refreshCloud(); await refreshNearby() }
        case .loginError(let text):
            if signingIn { alert = text }
            signingIn = false; loginURL = nil
        default:
            guard event.session == session, connected != nil else { return }
            switch event.payload {
            case .log(let text):
                guard event.stream == logStream else { return }
                logs.append(text)
                if logs.count > 2000 { logs.removeFirst(logs.count - 2000) }
            case .video:
                cameraFeeds.first(where: { $0.stream == event.stream })?.receive(event)
            case .logsError(let text): if event.stream == logStream { logError = text }
            case .cameraError: cameraFeeds.first(where: { $0.stream == event.stream })?.receive(event)
            default: break
            }
        }
    }
}

struct AppDeletion {
    let app: String
    let device: String
    let session: UInt64
}
