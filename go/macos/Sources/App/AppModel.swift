#if os(macOS)
import AppKit
#else
import UIKit
#endif
import SwiftUI

@MainActor
final class AppModel: ObservableObject {
    var nearbySetupPromptHistory = NearbySetupPromptHistory()
    @Published var nearby: [Device] = []
    #if os(macOS)
    @Published var nearbyUnitree: [UnitreeRobot] = UnitreeSavedDevices.load()
    @Published var unitreeDiscoveryError: String?
    #endif
    @Published var cloud: [Device] = []
    @Published var selection: Device?
    @Published var requestedDevice: Device?
    var setupSessions = 0
    private var experienceObserver: UUID?
    @Published private(set) var experienceEvents: [ExperienceEvent] = []
    @Published var connected: Device?
    @Published private(set) var connectedLite = false
    @Published var appActions: Set<String> = []
    @Published var appDeletion: AppDeletion?
    @Published var apps: [DeviceApp] = []
    @Published var showHiddenApps = false
    @Published private var hiddenAppsByDevice = UserDefaults.standard.dictionary(forKey: "hiddenAppsByDevice") as? [String: [String]] ?? [:]

    var hiddenAppCount: Int { apps.filter { isAppHidden($0.appName) }.count }
    var visibleApps: [DeviceApp] { apps.filter { showHiddenApps || !isAppHidden($0.appName) } }
    func isAppHidden(_ app: String) -> Bool {
        guard let id = connected?.id else { return false }
        return hiddenAppsByDevice[id]?.contains(app) == true
    }
    func setAppHidden(_ app: String, hidden: Bool) {
        guard let id = connected?.id else { return }
        var names = Set(hiddenAppsByDevice[id] ?? [])
        if hidden { names.insert(app) } else { names.remove(app) }
        hiddenAppsByDevice[id] = names.sorted()
        UserDefaults.standard.set(hiddenAppsByDevice, forKey: "hiddenAppsByDevice")
    }
    private var cameraOrder = UserDefaults.standard.dictionary(forKey: "cameraOrderByDevice") as? [String: [String]] ?? [:]
    @Published var cameras: [Camera] = []
    @Published var logs: [LogEntry] = []
    @Published var cameraFeeds: [CameraFeed] = []
    @Published var logApp = ""
    @Published var connectingDevice: Device?
    @Published var failedConnection: Device?
    @Published var connectionError: String?
    private var connectionAttempt: UInt64 = 0
    private let connectDevice: (String, UInt64, Bool) async throws -> DeviceConnection

    init(connectDevice: @escaping (String, UInt64, Bool) async throws -> DeviceConnection = { selector, request, forgetIdentity in
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
    @Published var loginURL: URL?
    #if os(iOS)
    @Published var webpageURL: URL?
    @Published var mobileCloudAccounts: [SavedAccount] = []
    @Published var mobileCloudOrganizations: [CloudOrganization] = []
    @Published var mobileCloudAccount = ""
    @Published var mobileCloudOrganization = ""
    @Published var mobileCloudSelectionLoading = false
    @Published var mobileCloudSelectionError: String?
    @Published var mobileMeshState = "searching"
    @Published var mobileMeshDetail = "Searching for Wendy Net peers…"
    private var mobileRuntimeScope = ""
    private var mobileLifecycleGeneration = 0
    private var mobileWasBackgrounded = false
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
    private var cameraPreviewLimit: Int?
    private var cameraBatch: UInt64 = 0
    private var cameraRecoveryGeneration: UInt64 = 0
    private var starting = false
    private var shuttingDown = false
    let cleanup = AsyncActions()

    func start() async {
        guard !initialized, !starting, ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] == nil else { return }
        starting = true
        defer { starting = false }
        do {
            #if os(iOS)
            try await client.initializeMobile()
            try await client.setMobileState(active: false, account: "", organization: "")
            #else
            guard let cli = Bundle.main.url(forAuxiliaryExecutable: "wendy") else {
                throw WendyError(message: "The bundled Wendy CLI is missing. Rebuild the app with Scripts/build.sh.")
            }
            try await client.initialize(cliPath: cli.path)
            #endif
            initialized = true
            await refreshAccount()
            await refreshNearby()
            if account?.signedIn == true { await refreshCloud() }
        } catch { if !Task.isCancelled { alert = error.localizedDescription } }
    }

    /// Polling and account refreshes share the app task's lifetime.
    func observeEvents() async {
        guard initialized else { return }
        let refreshes = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
        async let polling: Void = pollEvents(refreshes.continuation)
        for await _ in refreshes.stream {
            await refreshAccount()
            #if os(iOS)
            // Signing in again can keep identical account display metadata.
            // Refresh organizations even when the account task's ID is unchanged.
            await refreshMobileCloudSelection()
            #endif
            await refreshCloud()
            await refreshNearby()
        }
        await polling
    }

    private func pollEvents(_ refreshes: AsyncStream<Void>.Continuation) async {
        defer { refreshes.finish() }
        while !Task.isCancelled && !shuttingDown {
            do {
                for event in try await client.poll() {
                    try Task.checkCancellation()
                    guard !shuttingDown else { return }
                    receive(event, refreshes: refreshes)
                }
                try await Task.sleep(for: .milliseconds(16))
            } catch {
                if !Task.isCancelled && !shuttingDown { alert = error.localizedDescription }
                return
            }
        }
    }

    func refreshAccount() async {
        guard initialized else { return }
        do { account = try await client.account(); accountError = nil }
        catch { if !Task.isCancelled { accountError = error.localizedDescription } }
    }

    func refreshNearby() async {
        guard initialized, !discovering else { return }
        discovering = true
        defer { discovering = false }
        #if os(macOS)
        async let robots = UnitreeClient.shared.robots()
        #endif
        do {
            #if os(iOS)
                let scope = mobileCloudSelectionKey
                let devices = try await client.nearby()
                guard scope == mobileCloudSelectionKey, !Task.isCancelled else { return }
                nearby = devices
            #else
                nearby = try await client.nearby()
            #endif
            nearbyError = nil
        }
        catch { if !Task.isCancelled { nearbyError = error.localizedDescription } }
        #if os(macOS)
        do {
            let discovered = try await robots.robots
            nearbyUnitree = UnitreeSavedDevices.merge(saved: UnitreeSavedDevices.load(), discovered: discovered)
            unitreeDiscoveryError = nil
        }
        catch { if !Task.isCancelled { nearbyUnitree = UnitreeSavedDevices.load(); unitreeDiscoveryError = error.localizedDescription } }
        #endif
    }

    func refreshCloud() async {
        guard initialized, !discovering else { return }
        discovering = true
        defer { discovering = false }
        do {
            #if os(iOS)
                let scope = mobileCloudSelectionKey
                let devices = try await client.cloud()
                guard scope == mobileCloudSelectionKey, !Task.isCancelled else { return }
                cloud = devices
            #else
                cloud = try await client.cloud()
            #endif
            cloudError = nil
        }
        catch { if !Task.isCancelled { cloudError = error.localizedDescription } }
    }

    func connect(_ device: Device, forgetIdentity: Bool = false, force: Bool = false) async {
        let attempt = client.beginConnection()
        connectionAttempt = attempt
        selection = device
        detailError = nil
        failedConnection = nil; connectionError = nil
        canForgetIdentity = false
        if connected?.id == device.id && !forgetIdentity && !force {
            connectingDevice = nil
            return
        }
        connectingDevice = device
        do {
            let connection = try await connectDevice(device.selector, attempt, forgetIdentity)
            guard connectionAttempt == attempt else { return }
            let newSession = connection.session
            connectedLite = connection.lite
            cameraFeeds.forEach { $0.pause() }
            cameraFeeds = []; cameras = []; apps = []; logs = []
            deviceChat.stop()
            deviceChat = DeviceChatModel(session: newSession)
            session = newSession
            var resolvedDevice = device
            resolvedDevice.lite = connection.lite
            selection = resolvedDevice; connected = resolvedDevice
            recordExperience("Connected", symbol: "checkmark.circle", device: resolvedDevice)
            detailError = nil; appsError = nil; camerasError = nil; logError = nil
            connectingDevice = nil
            await refreshDetails()
        } catch {
            guard connectionAttempt == attempt else { return }
            connectingDevice = nil
            guard !Task.isCancelled else { return }
            canForgetIdentity = (error as? WendyError)?.canForgetIdentity ?? false
            if connected?.id == device.id { connected = nil; connectedLite = false }
            failedConnection = device
            let raw = error.localizedDescription
            let denied = raw.lowercased().contains("no route to host") ? await LocalNetworkAccess.denied(selector: device.selector) : false
            guard connectionAttempt == attempt, !Task.isCancelled else { return }
            detailError = denied ? "Local network access denied by macOS. " + raw : raw
        }
    }

    func retryConnection(_ device: Device) async {
        let previous = connectionAttempt
        connectingDevice = device
        detailError = nil
        do {
            let refreshed = try await client.nearby()
            guard connectionAttempt == previous, !Task.isCancelled else { return }
            nearby = refreshed
            let current = refreshed.first { $0.id == device.id } ?? device
            await connect(current, force: true)
        } catch {
            guard connectionAttempt == previous, !Task.isCancelled else { return }
            connectingDevice = nil
            detailError = error.localizedDescription
        }
    }

    private func recordExperience(_ title: String, symbol: String, device: Device) {
        if let recent = experienceEvents.first,
           recent.deviceID == device.id && recent.title == title && Date.now.timeIntervalSince(recent.date) < 60 { return }
        experienceEvents.insert(ExperienceEvent(deviceID: device.id, deviceName: device.title, title: title, symbol: symbol), at: 0)
        experienceEvents = Array(experienceEvents.prefix(100))
    }

    /// The visible app owns this loop; backgrounding cancels reads and retries.
    /// Trust, sign-in, and permission failures always remain explicit decisions.
    func maintainExperience() async {
        let observer = UUID()
        experienceObserver = observer
        defer { if experienceObserver == observer { experienceObserver = nil } }
        var failures = 0
        var lastDiscovery = Date.distantPast
        while !Task.isCancelled && initialized && experienceObserver == observer {
            do { try await Task.sleep(for: .seconds(failures == 0 ? 15 : min(30, pow(2, Double(min(failures, 5)))))) } catch { return }
            guard !Task.isCancelled, experienceObserver == observer else { return }
            guard setupSessions == 0 else { continue }
            if let device = selection, connectingDevice == nil,
               let error = detailError, ConnectionRecovery.canRetry(error) {
                await retryConnection(device)
                failures = detailError == nil ? 0 : failures + 1
            } else if selectedDeviceReady {
                let capturedSession = session
                let capturedSelection = selection?.id
                await refreshDetails()
                guard !Task.isCancelled, experienceObserver == observer, setupSessions == 0,
                      session == capturedSession, selection?.id == capturedSelection else { continue }
                if let device = selection, let appError = appsError, let cameraError = camerasError,
                   ConnectionRecovery.canRetry(appError), ConnectionRecovery.canRetry(cameraError) {
                    await connect(device, force: true)
                }
            }
            guard !Task.isCancelled else { return }
            if Date.now.timeIntervalSince(lastDiscovery) >= 30 {
                await refreshNearby()
                if account?.signedIn == true { await refreshCloud() }
                lastDiscovery = .now
            }
        }
    }

    func openApp(_ app: DeviceApp) async {
        guard connected != nil else { return }
        let capturedSession = session
        do {
            let url = try await client.openApp(app.appName, session: capturedSession)
            guard session == capturedSession, !Task.isCancelled else { return }
            #if os(macOS)
            if !NSWorkspace.shared.open(url) { alert = "Could not open the app webpage in your browser." }
            #else
            webpageURL = url
            #endif
        } catch {
            guard session == capturedSession, !Task.isCancelled else { return }
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
            guard session == capturedSession, !Task.isCancelled else { return }
            let updated = try await client.apps()
            guard session == capturedSession, !Task.isCancelled else { return }
            apps = updated
        } catch {
            guard session == capturedSession, !Task.isCancelled else { return }
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
            let result = connectedLite ? [] : try await client.apps()
            guard session == capturedSession, !Task.isCancelled else { return }
            let previous = Dictionary(uniqueKeysWithValues: apps.map { ($0.id, $0.runningState) })
            if let connected {
                for app in result where previous[app.id] != nil && previous[app.id] != app.runningState {
                    recordExperience("\(app.appName) · \(app.state)", symbol: "square.stack.3d.up", device: connected)
                }
            }
            apps = result; appsError = nil
        } catch {
            guard session == capturedSession, !Task.isCancelled else { return }
            appsError = error.localizedDescription
        }
        do {
            let result = try await client.cameras()
            guard session == capturedSession, !Task.isCancelled else { return }
            cameras = result
            let previous = Dictionary(uniqueKeysWithValues: cameraFeeds.map { ($0.camera.id, $0) })
            for feed in cameraFeeds where !cameras.contains(feed.camera) {
                pauseCamera(feed, manually: false)
            }
            cameraFeeds = cameras.map { camera in
                if let feed = previous[camera.id], feed.camera == camera { return feed }
                return CameraFeed(camera: camera)
            }
            applyCameraOrder()
            camerasError = nil
            if cameraWallVisible { await playCameras() }
        }
        catch { if session == capturedSession && !Task.isCancelled { camerasError = error.localizedDescription } }
    }

    func moveCamera(_ source: String, before target: String) {
        guard let device = connected, source != target,
              let from = cameraFeeds.firstIndex(where: { $0.camera.orderKey == source }),
              cameraFeeds.contains(where: { $0.camera.orderKey == target }) else { return }
        let moving = cameraFeeds.remove(at: from)
        let to = cameraFeeds.firstIndex(where: { $0.camera.orderKey == target }) ?? cameraFeeds.endIndex
        cameraFeeds.insert(moving, at: to)
        cameraOrder[device.id] = cameraFeeds.map { $0.camera.orderKey }
        UserDefaults.standard.set(cameraOrder, forKey: "cameraOrderByDevice")
    }

    private func applyCameraOrder() {
        guard let device = connected, let order = cameraOrder[device.id] else { return }
        cameraFeeds = cameraFeeds.enumerated().sorted { a, b in
            let ai = order.firstIndex(of: a.element.camera.orderKey) ?? order.count + a.offset
            let bi = order.firstIndex(of: b.element.camera.orderKey) ?? order.count + b.offset
            return ai < bi
        }.map(\.element)
    }

    func resources() async throws -> ResourceSnapshot { try await client.resources(session: session) }
    func ports(app: String) async throws -> [ResourcePort] { try await client.ports(app: app, session: session) }

    func streamLogs(clear: Bool = true) async {
        guard !Task.isCancelled else { return }
        if clear { logs = [] }
        logError = nil
        logStream &+= 1
        let stream = logStream
        do { try await client.startLogs(app: logApp, stream: stream) }
        catch { if logStream == stream && !Task.isCancelled { logError = error.localizedDescription } }
    }

    func observeLogs() async {
        await streamLogs()
        var ownedStream = logStream
        let capturedSession = session
        defer {
            let closingStream = ownedStream
            cleanup.submit { [weak self] in
                guard let self, self.logStream == closingStream else { return }
                try? await self.client.stopLogs()
            }
        }
        while !Task.isCancelled && session == capturedSession && logStream == ownedStream {
            do { try await Task.sleep(for: .seconds(5)) } catch { return }
            guard !Task.isCancelled, session == capturedSession, logStream == ownedStream else { return }
            if let error = logError, ConnectionRecovery.canRetry(error) {
                await streamLogs(clear: false)
                ownedStream = logStream
            }
        }
    }

    func showCameraWall(limit: Int? = nil) async {
        cameraWallVisible = true
        cameraPreviewLimit = limit
        for feed in cameraFeeds.prefix(limit ?? cameraFeeds.count) { feed.allowAutoplay() }
        cameraRecoveryGeneration &+= 1
        let generation = cameraRecoveryGeneration
        await playCameras()
        // The camera wall's view task owns retries and cancels on dismissal.
        while !Task.isCancelled {
            do { try await Task.sleep(for: .seconds(2)) } catch { return }
            guard cameraWallVisible, cameraRecoveryGeneration == generation else { return }
            let expectedSession = session
            for feed in cameraFeeds.prefix(cameraPreviewLimit ?? cameraFeeds.count) where feed.shouldAutoStart(at: Date()) {
                guard cameraWallVisible, cameraRecoveryGeneration == generation,
                      session == expectedSession, !Task.isCancelled else { return }
                await playCamera(feed)
            }
        }
    }

    func hideCameraWall() {
        cameraWallVisible = false
        cameraRecoveryGeneration &+= 1
        pauseCameras()
    }

    func playCameras() async {
        cameraBatch &+= 1
        let batch = cameraBatch
        let currentSession = session
        for feed in cameraFeeds.prefix(cameraPreviewLimit ?? cameraFeeds.count) {
            guard cameraWallVisible, cameraBatch == batch, session == currentSession, !Task.isCancelled else { return }
            if !feed.running && !feed.manuallyPaused { await playCamera(feed) }
        }
    }

    func playCamera(_ feed: CameraFeed, inline: Bool = false) async {
        _ = await playOwnedCamera(feed, inline: inline)
    }

    /// A presenting view may replace another view's stream. Return its token so
    /// dismissal cleanup cannot stop the newer owner's camera subscription.
    func playOwnedCamera(_ feed: CameraFeed, inline: Bool = false) async -> UInt64 {
        guard connected != nil, (cameraWallVisible || inline),
              cameraFeeds.contains(where: { $0 === feed }) else { return 0 }
        cameraStream &+= 1
        let stream = cameraStream
        feed.start(stream: stream)
        let currentSession = session
        do {
            try await client.startCamera(id: feed.camera.id, stream: stream, session: currentSession, stableID: feed.camera.stableID)
        } catch {
            if Task.isCancelled {
                if feed.stream == stream { feed.pause() }
                try? await client.stopCamera(id: feed.camera.id, stream: stream, session: currentSession)
            } else { feed.fail(error.localizedDescription, stream: stream) }
        }
        return stream
    }

    func pauseCamera(_ feed: CameraFeed, manually: Bool = true) {
        let stream = feed.stream
        feed.pause(manually: manually)
        guard stream != 0 else { return }
        let currentSession = session
        // The app lifetime owns these cleanup actions, including after a camera
        // view disappears. Shutdown joins them before releasing the backend.
        cleanup.submit { [client] in
            try? await client.stopCamera(id: feed.camera.id, stream: stream, session: currentSession)
        }
    }

    func pauseCameras() {
        cameraBatch &+= 1
        for feed in cameraFeeds { pauseCamera(feed, manually: false) }
    }

    func signIn(development: Bool = false) async {
        guard !signingIn else { return }
        signingIn = true
        do { try await client.login(development: development) }
        catch { signingIn = false; if !Task.isCancelled { alert = error.localizedDescription } }
    }

    func cancelLogin() async {
        do { try await client.cancelLogin(); signingIn = false; loginURL = nil }
        catch { if !Task.isCancelled { alert = error.localizedDescription } }
    }

    func enableVPN() async {
        guard !vpnUpdating else { return }
        vpnError = nil
        // Check prerequisites before stopping a working tunnel or opening Go relays.
        if let issue = vpn.setupIssue { vpnError = issue; return }
        vpnUpdating = true
        defer { vpnUpdating = false }
        do {
            try await vpn.stop()
            let config = try await client.startVPN(routes: [], gateway: "auto")
            do { try await vpn.start(config) }
            catch { try? await client.stopVPN(); throw error }
        } catch {
            if Task.isCancelled {
                try? await vpn.stop()
                try? await client.stopVPN()
            } else { vpnError = error.localizedDescription }
        }
    }

    func disableVPN() async {
        guard !vpnUpdating else { return }
        vpnError = nil
        vpnUpdating = true
        defer { vpnUpdating = false }
        do { try await vpn.stop(); try await client.stopVPN() }
        catch { if !Task.isCancelled { vpnError = error.localizedDescription } }
    }

    func shutdown() async {
        shuttingDown = true
        deviceChat.stop()
        hideCameraWall()
        await cleanup.finishAndWait()
        try? await vpn.stop()
        try? await client.shutdown()
        initialized = false
    }

    private func receive(_ event: BridgeEvent, refreshes: AsyncStream<Void>.Continuation) {
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
            refreshes.yield(())
        case .loginError(let text):
            if signingIn { alert = text }
            signingIn = false; loginURL = nil
        case .vpnError(let text):
            if vpn.enabled && !vpnUpdating { vpnError = text }
        default:
            guard event.session == session, connected != nil else { return }
            switch event.payload {
            case .log(let text):
                guard event.stream == logStream else { return }
                logs.append(text)
                if logs.count > 2000 { logs.removeFirst(logs.count - 2000) }
            case .video, .jpeg, .thermal:
                cameraFeeds.first(where: { $0.stream == event.stream })?.receive(event)
            case .logsError(let text): if event.stream == logStream { logError = text }
            case .cameraError: cameraFeeds.first(where: { $0.stream == event.stream })?.receive(event)
            default: break
            }
        }
    }
}

#if os(iOS)
    extension AppModel {
        // Owned by the root scene, independently of the selected device pane.
        func runMobileMesh(active: Bool) async {
            guard initialized else { return }
            mobileLifecycleGeneration += 1
            let generation = mobileLifecycleGeneration
            let scope = mobileCloudSelectionKey
            let changed = scope != mobileRuntimeScope
            let reconnect = !changed && mobileWasBackgrounded ? selection : nil
            if !active || changed {
                deviceChat.stop()
                hideCameraWall()
                connectionAttempt = client.beginConnection()
                connected = nil
                connectedLite = false
                connectingDevice = nil
                apps = []
                cameraFeeds = []
                if changed {
                    selection = nil
                    nearby = []
                    cloud = []
                }
            }
            do {
                try await client.setMobileState(
                    active: active,
                    account: mobileCloudAccount,
                    organization: mobileCloudOrganization
                )
                guard generation == mobileLifecycleGeneration, !Task.isCancelled else { return }
                mobileRuntimeScope = scope
                mobileWasBackgrounded = !active
                if !active {
                    mobileMeshState = "paused"
                    mobileMeshDetail = "Wendy Net paused in the background."
                    return
                }
                await refreshAccount()
                if changed { await refreshCloud() }
                if let reconnect {
                    if reconnect.transport == .mesh {
                        // A restarted router must obtain new signed routes before
                        // reconnecting. A saved selection is not reachability proof.
                        let deadline = ContinuousClock.now + .seconds(90)
                        while ContinuousClock.now < deadline {
                            try Task.checkCancellation()
                            guard generation == mobileLifecycleGeneration,
                                selection?.id == reconnect.id else { return }
                            await refreshNearby()
                            if nearby.contains(where: { $0.selector == reconnect.selector }) {
                                await connect(reconnect, force: true)
                                break
                            }
                            try await Task.sleep(for: .seconds(2))
                        }
                    } else {
                        await connect(reconnect, force: true)
                    }
                }
                while !Task.isCancelled {
                    let summary = try await client.mobileMeshSummary()
                    try Task.checkCancellation()
                    guard generation == mobileLifecycleGeneration else { return }
                    mobileMeshState = summary.0
                    mobileMeshDetail = summary.1
                    await refreshNearby()
                    try await Task.sleep(for: .seconds(5))
                }
            } catch {
                if !Task.isCancelled && generation == mobileLifecycleGeneration {
                    mobileMeshState = "unavailable"
                    mobileMeshDetail = error.localizedDescription
                    nearbyError = error.localizedDescription
                }
            }
        }
    }
#endif

struct AppDeletion {
    let app: String
    let device: String
    let session: UInt64
}
