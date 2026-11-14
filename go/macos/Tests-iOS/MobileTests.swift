import XCTest
import ARKit
import RoomPlan
import AVFoundation

@testable import WendyMobile

final class MobileTests: XCTestCase {
    func testEmbeddedSignInProducesBrowserURL() async throws {
        let client = GoClient.shared
        try await initializeIsolatedSandbox()
        try await client.login(development: false)
        do {
            var loginURL: URL?
            for _ in 0..<100 {
                for event in try await client.poll() {
                    switch event.payload {
                    case .loginURL(let value): loginURL = URL(string: value)
                    case .loginError(let error): throw error
                    default: break
                    }
                }
                if loginURL != nil { break }
                try await Task.sleep(for: .milliseconds(50))
            }
            let url = try XCTUnwrap(loginURL, "Sign-in must emit a URL for the Safari sheet")
            XCTAssertEqual(url.scheme, "https")
            XCTAssertTrue(url.path.hasSuffix("/authorize"))
            try await client.cancelLogin()
        } catch {
            try? await client.cancelLogin()
            throw error
        }
    }

    @MainActor
    func testOrganizationFailureKeepsSavedAccountAndRetryRecovers() async {
        let model = AppModel()
        let saved = SavedAccount(account: CloudAccount(
            state: WD_ACCOUNT_SIGNED_IN, context: "test-account",
            cloud: "https://cloud.example", email: "test@example.com"
        ), active: true)
        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { account in
                XCTAssertEqual(account, saved.id)
                XCTAssertEqual(model.mobileCloudAccounts.map(\.id), [saved.id])
                throw WendyError(message: "Organization service unavailable")
            }
        )
        XCTAssertEqual(model.mobileCloudAccounts.map(\.id), [saved.id])
        XCTAssertEqual(model.mobileCloudAccount, saved.id)
        XCTAssertEqual(model.mobileCloudSelectionError, "Organization service unavailable")
        XCTAssertFalse(model.mobileCloudSelectionLoading)

        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { _ in [CloudOrganization(id: "org", name: "Test organization")] }
        )
        XCTAssertEqual(model.mobileCloudOrganization, "org")
        XCTAssertNil(model.mobileCloudSelectionError)
        XCTAssertFalse(model.mobileCloudSelectionLoading)
    }

    @MainActor
    func testAccountChangeClearsPreviousOrganizationBeforeFailedRead() async {
        let model = AppModel()
        model.mobileCloudAccount = "old-account"
        model.mobileCloudOrganization = "old-org"
        model.mobileCloudOrganizations = [CloudOrganization(id: "old-org", name: "Old organization")]
        let saved = SavedAccount(account: CloudAccount(
            state: WD_ACCOUNT_SIGNED_IN, context: "new-account",
            cloud: "https://cloud.example", email: "test@example.com"
        ), active: true)
        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { _ in throw WendyError(message: "Offline") }
        )
        XCTAssertEqual(model.mobileCloudAccount, saved.id)
        XCTAssertTrue(model.mobileCloudOrganizations.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
        XCTAssertEqual(model.mobileCloudSelectionError, "Offline")
    }

    @MainActor
    func testSignedOutRefreshClearsOrganizationWithoutCloudRequest() async {
        let model = AppModel()
        model.mobileCloudOrganization = "stale-org"
        model.mobileCloudOrganizations = [CloudOrganization(id: "stale-org", name: "Stale organization")]
        await model.refreshMobileCloudSelection(
            loadAccounts: { [] },
            loadOrganizations: { _ in XCTFail("Signed-out refresh should not call Cloud"); return [] }
        )
        XCTAssertTrue(model.mobileCloudAccounts.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganizations.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
        XCTAssertNil(model.mobileCloudSelectionError)
    }

    @MainActor
    func testSavedCertificateScopeIsReadyBeforeOrganizationLookupFinishes() async {
        let model = AppModel()
        let saved = SavedAccount(account: CloudAccount(state: WD_ACCOUNT_SIGNED_IN,
            context: "local-account", cloud: "https://cloud.example", email: "test@example.com"),
            active: true, organizationID: "tenant", organizationName: "Local organization")
        await model.refreshMobileCloudSelection(loadAccounts: { [saved] }, loadOrganizations: { _ in
            // The network request has not returned, yet consumers can use the scope.
            XCTAssertEqual(model.mobileCloudAccount, saved.id)
            XCTAssertEqual(model.mobileCloudOrganization, "tenant")
            XCTAssertEqual(model.mobileCloudOrganizations.first?.name, "Local organization")
            throw WendyError(message: "Timed out")
        })
        XCTAssertEqual(model.mobileCloudOrganization, "tenant")
        XCTAssertEqual(model.mobileCloudSelectionError, "Timed out")
    }

    @MainActor
    func testLocalScopeSwitchNeverReusesPreviousTenant() async {
        let model = AppModel()
        model.mobileCloudAccount = "old"
        model.mobileCloudOrganization = "old-tenant"
        let saved = SavedAccount(account: CloudAccount(state: WD_ACCOUNT_SIGNED_IN,
            context: "new", cloud: "https://cloud.example", email: "test@example.com"),
            active: true, organizationID: "new-tenant")
        await model.refreshMobileCloudSelection(loadAccounts: { [saved] }, loadOrganizations: { _ in
            XCTAssertEqual(model.mobileCloudOrganization, "new-tenant")
            XCTAssertEqual(model.mobileCloudOrganizations.map(\.id), ["new-tenant"])
            return []
        })
        XCTAssertEqual(model.mobileCloudOrganization, "new-tenant")
    }

    @MainActor
    func testLoginRequiredDoesNotBootstrapCertificateScope() async {
        let model = AppModel()
        let saved = SavedAccount(account: CloudAccount(state: WD_ACCOUNT_NEEDS_LOGIN,
            context: "expired", cloud: "https://cloud.example", email: "test@example.com"),
            active: true, organizationID: "tenant")
        await model.refreshMobileCloudSelection(loadAccounts: { [saved] }, loadOrganizations: { _ in
            XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
            throw WendyError(message: "Sign in again")
        })
        XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
    }

    func testAccountReadsDoNotWaitForDeviceRequestQueue() async throws {
        try await initializeIsolatedSandbox()
        let client = GoClient.shared
        let started = expectation(description: "Device request occupies its queue")
        let readFinished = expectation(description: "Account reads finish independently")
        let release = DispatchSemaphore(value: 0)
        async let blocked: Void = client.perform {
            started.fulfill()
            _ = release.wait(timeout: .now() + 5)
        }
        await fulfillment(of: [started], timeout: 2)
        @Sendable func readAccounts() async throws {
            _ = try await client.account()
            _ = try await client.accounts()
            readFinished.fulfill()
        }
        async let reading: Void = readAccounts()
        await fulfillment(of: [readFinished], timeout: 2)
        release.signal()
        _ = try await (blocked, reading)
    }

    private func initializeIsolatedSandbox(isolation: isolated (any Actor)? = #isolation) async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("WendyTests-\(UUID().uuidString)", isDirectory: true)
        addTeardownBlock {
            try await GoClient.shared.setMobileState(active: false, account: "", organization: "")
            try FileManager.default.removeItem(at: directory)
        }
        try await GoClient.shared.setMobileState(active: false, account: "", organization: "")
        try await GoClient.shared.initializeMobile(directory: directory)
    }

    func testEmbeddedGoInitializesInAppSandbox() async throws {
        try await initializeIsolatedSandbox()
        let account = try await GoClient.shared.account()
        XCTAssertNotNil(account.title)
        // Polling crosses the C archive without a CLI process or device connection.
        let events = try await GoClient.shared.poll()
        XCTAssertTrue(events.isEmpty)
    }

    func testEmbeddedMeshLifecycleWithoutSavedAccount() async throws {
        try await initializeIsolatedSandbox()
        try await GoClient.shared.setMobileState(active: false, account: "", organization: "")
        try await GoClient.shared.setMobileState(active: true, account: "", organization: "")
        let summary = try await GoClient.shared.mobileMeshSummary()
        XCTAssertEqual(summary.0, "signed-out")
        XCTAssertFalse(summary.1.isEmpty)
        try await GoClient.shared.setMobileState(active: false, account: "", organization: "")
    }

    @MainActor
    func testMobileInventoriesLoadIndependentlyAndDiscardAnOldScope() async throws {
        try await initializeIsolatedSandbox()
        let model = AppModel()
        model.initialized = true
        await model.runMobileMesh(active: false)
        let started = expectation(description: "Nearby discovery in flight")
        let finish = AsyncCallback<(devices: [Device], warning: WendyError?)>()
        async let nearby: Void = model.refreshNearby {
            started.fulfill()
            return try await finish.value()
        }
        await fulfillment(of: [started], timeout: 2)
        XCTAssertTrue(model.nearbyLoading)
        let device = Device(id: "cloud", name: "Cloud", selector: "cloud", transport: .cloud, address: "")
        await model.refreshCloud { [device] }
        XCTAssertEqual(model.cloud.map(\.id), [device.id])
        XCTAssertTrue(model.cloudLoaded)
        XCTAssertFalse(model.cloudLoading)

        model.mobileCloudOrganization = "other"
        await model.runMobileMesh(active: false)
        finish.finish(.failure(WendyError(message: "Old network error")))
        await nearby
        XCTAssertNil(model.nearbyError)
        XCTAssertFalse(model.nearbyLoaded)
        XCTAssertFalse(model.nearbyLoading)
        XCTAssertTrue(model.cloud.isEmpty)
        XCTAssertFalse(model.cloudLoaded)
    }

    @MainActor
    func testQueuedAppControlsCannotTargetAReplacementSession() async {
        let device = Device(id: "other", name: "Other device", selector: "other", transport: .lan, address: "")
        let model = AppModel { _, _, _ in DeviceConnection(session: 72, lite: false) }
        await model.connect(device)
        let app = DeviceApp(appName: "dashboard", runningState: .running, services: [], httpPort: 8080)

        await model.setAppRunning(app.appName, running: false, expectedSession: 71)
        await model.restartApp(app.appName, expectedSession: 71)
        await model.openApp(app, expectedSession: 71)

        XCTAssertEqual(model.deviceSession, 72)
        XCTAssertNil(model.webpage)
        XCTAssertNil(model.alert)
        XCTAssertTrue(model.appActions.isEmpty)
    }

    @MainActor
    func testDeviceSwitchDismissesOpenDashboardWhileReplacementIsConnecting() async throws {
        let first = Device(id: "a", name: "First", selector: "a", transport: .lan, address: "")
        let second = Device(id: "b", name: "Second", selector: "b", transport: .cloud, address: "")
        let dialStarted = expectation(description: "Replacement dial started")
        let finishDial = AsyncCallback<DeviceConnection>()
        let model = AppModel { device, _, _ in
            if device.id == first.id { return DeviceConnection(session: 31, lite: false) }
            dialStarted.fulfill()
            return try await finishDial.value()
        }
        await model.connect(first)
        model.webpage = MobileAppWebpage(url: try XCTUnwrap(URL(string: "http://127.0.0.1:8000")),
                                         app: "dashboard", device: first.title)
        async let switchDevice: Void = model.connect(second)
        await fulfillment(of: [dialStarted], timeout: 2)
        XCTAssertNil(model.webpage)
        XCTAssertFalse(model.selectedDeviceReady)
        finishDial.finish(.failure(WendyError(message: "Unavailable")))
        await switchDevice
        XCTAssertNil(model.webpage)
        XCTAssertFalse(model.selectedDeviceReady)
    }

    @MainActor
    func testLateDeviceConnectionCannotReplaceNewSelection() async throws {
        let first = Device(id: "a", name: "Device A", selector: "a", transport: .lan, address: "")
        let second = Device(id: "b", name: "Device B", selector: "b", transport: .cloud, address: "")
        let started = expectation(description: "Device A is connecting")
        let finish = AsyncCallback<DeviceConnection>()
        let model = AppModel { device, _, _ in
            if device.id == first.id {
                started.fulfill()
                return try await finish.value()
            }
            return DeviceConnection(session: 52, lite: false)
        }

        async let firstConnection: Void = model.connect(first)
        await fulfillment(of: [started], timeout: 2)
        await model.connect(second)
        finish.finish(.success(DeviceConnection(session: 51, lite: false)))
        await firstConnection

        XCTAssertEqual(model.selection?.id, second.id)
        XCTAssertEqual(model.connected?.id, second.id)
        XCTAssertEqual(model.deviceSession, 52)
        XCTAssertTrue(model.selectedDeviceReady)
    }

    @MainActor
    func testLateLogsFromPreviousDeviceAreIgnoredAndCleanupRetainsItsOwner() async throws {
        let first = Device(id: "first", name: "First", selector: "first", transport: .lan, address: "")
        let second = Device(id: "second", name: "Second", selector: "second", transport: .cloud, address: "")
        let model = AppModel { device, _, _ in
            DeviceConnection(session: device.id == first.id ? 41 : 42, lite: false)
        }
        let sink = AsyncStream<Void>.makeStream().continuation
        let entry = LogEntry(timestampNanoseconds: 1, level: .info, body: "device log", app: "", service: "")
        let firstStart = expectation(description: "Starting first log stream")
        let finishFirst = AsyncCallback<Void>()
        let firstToken = AsyncCallback<UInt64>()
        await model.connect(first)
        model.logApp = "old-filter"
        async let firstObserver: Void = model.observeLogs(start: { app, stream, session in
            XCTAssertEqual(app, "old-filter")
            XCTAssertEqual(session, 41)
            firstToken.finish(.success(stream))
            firstStart.fulfill()
            _ = try await finishFirst.value()
        }, stop: { stream, session in
            let token = try await firstToken.value()
            XCTAssertEqual(stream, token)
            XCTAssertEqual(session, 41)
        })
        await fulfillment(of: [firstStart], timeout: 2)
        let firstStream = try await firstToken.value()
        model.receive(.init(session: 41, stream: firstStream, payload: .log(entry)), refreshes: sink)
        XCTAssertEqual(model.logs.count, 1)

        await model.connect(second)
        XCTAssertTrue(model.logs.isEmpty)
        XCTAssertEqual(model.logApp, "")
        let secondStart = expectation(description: "Starting second log stream")
        let secondToken = AsyncCallback<UInt64>()
        await withTaskGroup(of: Void.self) { group in
            group.addTask {
                await model.observeLogs(start: { _, stream, session in
                    XCTAssertEqual(session, 42)
                    secondToken.finish(.success(stream))
                    secondStart.fulfill()
                }, stop: { stream, session in
                    let token = try await secondToken.value()
                    XCTAssertEqual(stream, token)
                    XCTAssertEqual(session, 42)
                })
            }
            await fulfillment(of: [secondStart], timeout: 2)
            let secondStream = (try? await secondToken.value()) ?? 0
            finishFirst.finish(.success(()))
            model.receive(.init(session: 41, stream: firstStream, payload: .log(entry)), refreshes: sink)
            model.receive(.init(session: 41, stream: firstStream, payload: .logsError("first failed")), refreshes: sink)
            XCTAssertTrue(model.logs.isEmpty)
            XCTAssertNil(model.logError)
            model.receive(.init(session: 42, stream: secondStream, payload: .log(entry)), refreshes: sink)
            XCTAssertEqual(model.logs.count, 1)
            group.cancelAll()
        }
        await firstObserver
        let secondStream = try await secondToken.value()
        model.receive(.init(session: 42, stream: secondStream, payload: .log(entry)), refreshes: sink)
        XCTAssertEqual(model.logs.count, 1)
    }

    @MainActor
    func testDeniedCloudConnectionWaitsForExplicitRetryAndRecovers() async {
        let device = Device(id: "remote", name: "Remote", selector: "remote", transport: .cloud, address: "")
        var attempts = 0
        let model = AppModel { _, _, _ in
            attempts += 1
            if attempts == 1 { throw WendyError(message: "PermissionDenied: unavailable to this account") }
            return DeviceConnection(session: 51, lite: false)
        }
        await model.connect(device)
        XCTAssertEqual(attempts, 1)
        XCTAssertFalse(model.selectedDeviceReady)
        XCTAssertFalse(ConnectionRecovery.canRetry(model.detailError ?? ""))
        XCTAssertEqual(PresentedFailure("Local network access denied.").title, "Allow local network access")
        XCTAssertTrue(PresentedFailure("Local network access denied.").message.contains("Privacy & Security"))
        await model.retryConnection(device)
        XCTAssertEqual(attempts, 2)
        XCTAssertTrue(model.selectedDeviceReady)
        XCTAssertEqual(model.deviceSession, 51)
    }

    @MainActor
    func testSignedOutSecureLANOffersSignInInsteadOfRetrying() async throws {
        try await initializeIsolatedSandbox()
        let model = AppModel()
        let device = Device(id: "enrolled", name: "Enrolled", selector: "127.0.0.1:50052",
                            transport: .lan, address: "127.0.0.1", requiresMTLS: true)
        await model.connect(device)
        let failure = try XCTUnwrap(model.detailError)
        XCTAssertFalse(ConnectionRecovery.canRetry(failure))
        XCTAssertTrue(PresentedFailure(failure).offersDeviceSignIn)
        XCTAssertEqual(PresentedFailure(failure).title, "Sign in to connect")
        XCTAssertNil(model.connectingDevice)
    }

    @MainActor
    func testNearbyPermissionDenialRetainsDiscoveredPeersAndExplicitRetryCanReturnEmpty() async throws {
        try await initializeIsolatedSandbox()
        let model = AppModel()
        model.initialized = true
        await model.runMobileMesh(active: false)
        let denial = "local network access denied (dns-sd error -65570)"
        await model.refreshNearby { throw WendyError(message: denial) }
        XCTAssertFalse(model.nearbyLoaded)
        XCTAssertEqual(model.nearbyError, denial)
        await model.refreshNearby(automatic: true) { XCTFail("Retried a denied browse"); return ([], nil) }
        let peer = Device(id: "usb", name: "Peer", selector: "usb", transport: .usb, address: "")
        await model.refreshNearby { ([peer], WendyError(message: denial)) }
        XCTAssertEqual(model.nearby.map(\.id), [peer.id])
        XCTAssertEqual(model.nearbyError, denial)
        await model.refreshNearby { ([], nil) }
        XCTAssertTrue(model.nearbyLoaded)
        XCTAssertTrue(model.nearby.isEmpty)
        XCTAssertNil(model.nearbyError)
    }

    @MainActor
    func testBackgroundDropsDeviceLogsAndForegroundReconnectsSelection() async throws {
        try await initializeIsolatedSandbox()
        let device = Device(id: "local", name: "Local", selector: "local", transport: .lan, address: "")
        let resumed = expectation(description: "Foreground reconnect")
        var attempts: UInt64 = 0
        let model = AppModel { selected, _, _ in
            XCTAssertEqual(selected.id, device.id)
            attempts += 1
            if attempts == 2 { resumed.fulfill() }
            return DeviceConnection(session: attempts, lite: false)
        }
        model.initialized = true
        await model.runMobileMesh(active: false)
        await model.connect(device)
        model.logs = [.init(timestampNanoseconds: 1, level: .info, body: "old", app: "", service: "")]
        await model.runMobileMesh(active: false)
        XCTAssertNil(model.connected)
        XCTAssertEqual(model.deviceSession, 0)
        XCTAssertEqual(model.selection?.id, device.id)
        XCTAssertTrue(model.logs.isEmpty)
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await model.runMobileMesh(active: true) }
            await fulfillment(of: [resumed], timeout: 5)
            group.cancelAll()
        }
        XCTAssertEqual(model.connected?.id, device.id)
        XCTAssertEqual(model.deviceSession, 2)
        XCTAssertTrue(model.logs.isEmpty)
    }

    // Explicit opt-in: uses the real app account and performs read-only management.
    // Ordinary test runs use disposable sandboxes and never join a production mesh.
    func testLiveMeshDiscoveryManagementAndResume() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard let tenant = environment["WENDY_LIVE_MESH_TENANT"], !tenant.isEmpty else {
            throw XCTSkip("Set WENDY_LIVE_MESH_TENANT after signing in on the test device.")
        }
        if environment["WENDY_LIVE_MESH_WAIT_FOR_READY"] == "1" {
            // Launch online for development-certificate validation, then let
            // the hardware harness signal over USB after isolating radios.
            let ready = FileManager.default.temporaryDirectory
                .appendingPathComponent("wendy-live-mesh-ready")
            try? FileManager.default.removeItem(at: ready)
            print("Live mesh waiting for USB ready marker before starting discovery.")
            let deadline = ContinuousClock.now + .seconds(180)
            while !FileManager.default.fileExists(atPath: ready.path) {
                guard ContinuousClock.now < deadline else {
                    XCTFail("Timed out waiting for hardware isolation.")
                    return
                }
                try await Task.sleep(for: .seconds(1))
            }
            try FileManager.default.removeItem(at: ready)
        }
        let client = GoClient.shared
        try await initializeIsolatedSandbox()
        let account = try await client.account()
        XCTAssertTrue(account.signedIn, "Sign in using the normal app before running live tests.")
        guard account.signedIn else { return }
        if let endpoint = environment["WENDY_LIVE_MESH_CLOUD"] {
            XCTAssertEqual(account.cloud, endpoint)
            guard account.cloud == endpoint else { return }
        }
        addTeardownBlock {
            try await client.setMobileState(active: false, account: "", organization: "")
        }
        try await client.setMobileState(
            active: true,
            account: account.context,
            organization: tenant
        )
        let device = try await discoverMeshTarget(
            tenant: tenant,
            selector: environment["WENDY_LIVE_MESH_SELECTOR"]
        )
        let connection = try await client.connect(
            selector: device.selector,
            request: client.beginConnection()
        )
        let info = try await client.deviceInfo(session: connection.session)
        print("Live mesh management: \(info.board), \(info.architecture), \(info.osVersion)")
        if !connection.lite && environment["WENDY_LIVE_MESH_NETWORK_SETTINGS"] == "1" {
            let settings = try await client.networkSettings(session: connection.session)
            print("Live mesh settings: mesh=\(settings.meshAvailable) runtime=\(settings.runtimeAvailable) topology=\(settings.topology.available) error=\(settings.meshError)")
            XCTAssertTrue(settings.meshAvailable, settings.meshError)
            XCTAssertTrue(settings.topology.available, settings.topology.error)
        }
        if connection.lite && environment["WENDY_LIVE_SENSORLINK_TEMPERATURE"] == "1" {
            try await verifyTemperature(session: connection.session)
        }
        if !connection.lite { _ = try await client.apps() }
        try await client.setMobileState(
            active: false,
            account: account.context,
            organization: tenant
        )
        let paused = try await client.mobileMeshSummary()
        XCTAssertEqual(paused.0, "paused")
        try await client.setMobileState(
            active: true,
            account: account.context,
            organization: tenant
        )
        _ = try await discoverMeshTarget(tenant: tenant, selector: device.selector)
        let resumed = try await client.connect(
            selector: device.selector,
            request: client.beginConnection()
        )
        _ = try await client.deviceInfo(session: resumed.session)
        if resumed.lite && environment["WENDY_LIVE_SENSORLINK_TEMPERATURE"] == "1" {
            try await verifyTemperature(session: resumed.session)
        }
        if !resumed.lite { _ = try await client.apps() }
        try await client.disconnect()
    }

    private func verifyTemperature(session: UInt64) async throws {
        var previous: UInt64 = 0
        let requested = Int(ProcessInfo.processInfo.environment["WENDY_LIVE_SENSORLINK_SAMPLES"] ?? "3") ?? 3
        let count = min(60, max(1, requested))
        for _ in 0..<count {
            let sample = try await GoClient.shared.sensorTemperature(session: session)
            XCTAssertTrue(sample.celsius.isFinite)
            XCTAssertGreaterThan(sample.timestampMicroseconds, previous)
            XCTAssertFalse(sample.name.isEmpty)
            print("Live SensorLink temperature: \(sample.celsius) C, source=\(sample.name), timestamp=\(sample.timestampMicroseconds)")
            previous = sample.timestampMicroseconds
            try await Task.sleep(for: .seconds(2))
        }
    }

    private func discoverMeshTarget(tenant: String, selector: String?) async throws -> Device {
        let requestedSelector = selector
        var selected: Device?
        var lastStatus = "Discovery has not run."
        let deadline = ContinuousClock.now + .seconds(90)
        while selected == nil, ContinuousClock.now < deadline {
            let nearby = try await GoClient.shared.nearby()
            let summary = try await GoClient.shared.mobileMeshSummary()
            lastStatus =
                "\(summary.0): \(summary.1); nearby: \(nearby.map { $0.transport.rawValue })"
            print("Live mesh status: \(lastStatus)")
            selected = nearby.first {
                $0.transport == .mesh && $0.selector.hasPrefix("mesh://\(tenant)/")
                    && (requestedSelector == nil || $0.selector == requestedSelector)
            }
            if selected == nil { try await Task.sleep(for: .seconds(2)) }
        }
        let device = try XCTUnwrap(
            selected,
            "No authenticated mesh target became reachable. \(lastStatus)"
        )
        print("Live mesh target: \(device.title) \(device.selector)")
        return device
    }

    func testLocalVideoParserAcceptsSplitStartCodes() {
        var parser = AnnexBParser()
        XCTAssertTrue(parser.append(Data([0, 0])).isEmpty)
        let nals = parser.append(Data([1, 0x65, 0x11, 0, 0, 1, 0x41]))
        XCTAssertEqual(nals, [Data([0x65, 0x11])])
    }

    func testRecordedVideoUsesEpisodeTimestampSchedule() {
        let times = [-2.0, -1.8, -1.5, 0.2]
        XCTAssertNil(DataVisualSearch.latest(times, at: -3, time: { $0 }))
        XCTAssertEqual(DataVisualSearch.latest(times, at: -1.6, time: { $0 }), -1.8)
        XCTAssertEqual(DataVisualSearch.latest(times, at: 2, time: { $0 }), 0.2)
    }
}

extension MobileTests {
    @MainActor
    func testChatUnreadRepliesFollowPresentationAndClear() {
        let chat = DeviceChatModel(session: 42)
        chat.setPresented(true)
        chat.receivedReply()
        XCTAssertEqual(chat.unreadReplies, 0)
        chat.setPresented(false)
        chat.receivedReply()
        XCTAssertEqual(chat.unreadReplies, 1)
        chat.setPresented(true)
        XCTAssertEqual(chat.unreadReplies, 0)
        chat.setPresented(false)
        chat.receivedReply()
        chat.clear()
        XCTAssertEqual(chat.unreadReplies, 0)
    }

    @MainActor
    func testChatStopInvalidatesPendingApprovalAndResponse() {
        let chat = DeviceChatModel(session: 42)
        chat.useModel("test-model", port: 11434)
        chat.prompt = "Which apps are running?"
        chat.send()
        XCTAssertTrue(chat.streaming)
        XCTAssertNotEqual(chat.turn, 0)
        chat.approval = ChatApproval(id: 1, action: "app_stop", app: "test-app")
        chat.stop()
        XCTAssertEqual(chat.turn, 0)
        XCTAssertNil(chat.approval)
        XCTAssertNil(chat.activeResponseID)
        XCTAssertFalse(chat.streaming)
        XCTAssertTrue(chat.messages.last?.interrupted == true)
    }
}
@MainActor
final class SiteRoomCaptureTests: XCTestCase {
    func testFinishThenDismissalStopsTheSessionOnlyOnce() {
        let controller = SiteRoomCaptureController()
        let session = RoomCaptureSessionSpy()
        XCTAssertTrue(controller.start(session))
        XCTAssertFalse(controller.start(session))
        controller.finish()
        controller.cancel() // Cancel or onDisappear.
        controller.cancel() // UIViewRepresentable dismantling.
        XCTAssertFalse(controller.start(session))
        XCTAssertEqual(session.starts, 1)
        XCTAssertEqual(session.stops, 1)
    }

    func testMemoryWarningThenDismissalStopsTheSessionOnlyOnce() {
        let controller = SiteRoomCaptureController()
        let session = RoomCaptureSessionSpy()
        controller.start(session)
        controller.memoryWarning()
        controller.cancel()
        controller.cancel()
        XCTAssertEqual(session.starts, 1)
        XCTAssertEqual(session.stops, 1)
        XCTAssertFalse(controller.shouldProcess(error: nil))
    }

    func testDismissalBeforeViewCreationNeverStartsTheSession() {
        let controller = SiteRoomCaptureController()
        let session = RoomCaptureSessionSpy()
        controller.cancel()
        XCTAssertFalse(controller.start(session))
        XCTAssertEqual(session.starts, 0)
        XCTAssertEqual(session.stops, 0)
    }

    func testMemoryWarningStopsCaptureWithoutStartingReconstruction() async {
        let controller = SiteRoomCaptureController()
        controller.memoryWarning()
        XCTAssertFalse(controller.scanning)
        XCTAssertFalse(controller.finishing)
        XCTAssertFalse(controller.shouldProcess(error: nil))
        // Dismissal and a queued Finish action must not replace the useful error.
        controller.cancel()
        controller.finish()
        XCTAssertFalse(controller.finishing)
        do {
            _ = try await controller.result.value()
            XCTFail("Expected a memory-pressure error")
        } catch {
            XCTAssertTrue(error is SiteRoomCaptureError)
            XCTAssertTrue(error.localizedDescription.contains("not saved"))
        }
    }

    func testFinishStillAllowsRoomProcessing() {
        let controller = SiteRoomCaptureController()
        controller.finish()
        controller.finish()
        XCTAssertFalse(controller.scanning)
        XCTAssertTrue(controller.finishing)
        XCTAssertTrue(controller.shouldProcess(error: nil))
        // A warning after Finish cannot undo reconstruction already underway.
        controller.memoryWarning()
        XCTAssertTrue(controller.finishing)
        XCTAssertTrue(controller.shouldProcess(error: nil))
        controller.cancel()
    }

    func testCancelRejectsQueuedRoomProcessing() async {
        let controller = SiteRoomCaptureController()
        controller.cancel()
        controller.finish()
        XCTAssertFalse(controller.scanning)
        XCTAssertFalse(controller.finishing)
        XCTAssertFalse(controller.shouldProcess(error: nil))
        do {
            _ = try await controller.result.value()
            XCTFail("Expected cancellation")
        } catch {
            XCTAssertTrue(error is CancellationError)
        }
    }

    func testRoomProcessingErrorReachesCaller() async {
        let controller = SiteRoomCaptureController()
        XCTAssertFalse(controller.shouldProcess(error: NSError(domain: "RoomCaptureTest", code: 7)))
        do {
            _ = try await controller.result.value()
            XCTFail("Expected the RoomPlan error")
        } catch {
            XCTAssertEqual((error as NSError).domain, "RoomCaptureTest")
            XCTAssertEqual((error as NSError).code, 7)
        }
        controller.cancel()
    }
}

// Opt-in physical-device probe. Simulator tests cannot exercise CoreMotion,
// LiDAR, or the RoomPlan camera startup from the reported crash.
@MainActor
final class PhysicalRoomCaptureTests: XCTestCase {
    private static func collectReferences(_ references: SiteReferenceCapture, vision: DeviceViewfinderController) async {
        await references.run(session: vision.session)
    }

    func testStartupStress() async throws {
        let mode = ProcessInfo.processInfo.environment["WENDY_ROOM_CAPTURE_REPRO"] ?? ""
        guard ["legacy", "roomplan"].contains(mode) else {
            throw XCTSkip("Set WENDY_ROOM_CAPTURE_REPRO to legacy or roomplan on a LiDAR device")
        }
        guard RoomCaptureSession.isSupported else { throw XCTSkip("Requires a physical LiDAR device") }
        let allowed = await AVCaptureDevice.requestAccess(for: .video)
        XCTAssertTrue(allowed)
        guard allowed else { return }
        let scene = try XCTUnwrap(UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.first)
        let previousWindow = scene.windows.first(where: \.isKeyWindow)
        let window = UIWindow(windowScene: scene)
        let host = UIViewController()
        window.rootViewController = host
        window.makeKeyAndVisible()
        defer { window.isHidden = true; previousWindow?.makeKeyAndVisible() }

        for cycle in 1...10 {
            let vision = DeviceViewfinderController()
            let references = SiteReferenceCapture()
            let delegate = RoomCaptureProbeDelegate()
            let view = RoomCaptureView(frame: host.view.bounds, arSession: vision.session)
            view.delegate = delegate
            host.view.addSubview(view)
            defer {
                references.stop()
                view.captureSession.stop()
                view.removeFromSuperview()
            }
            print("ROOM_REPRO start mode=\(mode) cycle=\(cycle)")
            view.captureSession.run(configuration: .init())
            print("ROOM_REPRO initial semantics=\(vision.session.configuration?.frameSemantics.rawValue ?? 0)")
            if mode == "legacy", let configuration = vision.session.configuration?.copy() as? ARWorldTrackingConfiguration {
                configuration.frameSemantics.insert(.sceneDepth)
                vision.session.run(configuration)
            }
            var cameraFrames = 0
            var depthFrames = 0
            await withTaskGroup(of: Void.self) { group in
                if mode == "legacy" { group.addTask { await vision.run(manageSession: false) } }
                group.addTask { await Self.collectReferences(references, vision: vision) }
                for _ in 0..<30 {
                    try? await Task.sleep(for: .milliseconds(100))
                    if let frame = vision.session.currentFrame {
                        cameraFrames += 1
                        if frame.sceneDepth != nil { depthFrames += 1 }
                    }
                }
                references.stop()
                group.cancelAll()
            }
            print("ROOM_REPRO completed mode=\(mode) cycle=\(cycle) cameraSamples=\(cameraFrames) depthSamples=\(depthFrames) landmarks=\(references.landmarkCount)")
            XCTAssertGreaterThan(cameraFrames, 0, "Camera never started in cycle \(cycle)")
            XCTAssertGreaterThan(depthFrames, 0, "RoomPlan did not supply scene depth in cycle \(cycle)")
        }
    }
}

@objc(WendyRoomCaptureProbeDelegate)
private final class RoomCaptureProbeDelegate: NSObject, RoomCaptureViewDelegate {
    override init() { super.init() }
    required init?(coder: NSCoder) { nil }
    func encode(with coder: NSCoder) {}
    func captureView(shouldPresent roomDataForProcessing: CapturedRoomData, error: Error?) -> Bool {
        if let error { print("ROOM_REPRO RoomPlan error: \(error)") }
        return false
    }
}

private final class RoomCaptureSessionSpy: SiteRoomCaptureSession {
    var starts = 0
    var stops = 0
    func run(configuration: RoomCaptureSession.Configuration) { starts += 1 }
    func stop() { stops += 1 }
}

final class MobileInventoryPresenceTests: XCTestCase {
    func testOnlyFreshVerifiedPresenceCanShowOnline() {
        let now = Date(timeIntervalSince1970: 1000)
        XCTAssertEqual(MobileInventoryPresence.cloud(online: true, verified: true, fetchedAt: now, now: now), .online)
        XCTAssertEqual(MobileInventoryPresence.cloud(online: false, verified: true, fetchedAt: now, now: now), .offline)
        XCTAssertEqual(MobileInventoryPresence.cloud(online: true, verified: false, fetchedAt: now, now: now), .unknown)
        XCTAssertEqual(MobileInventoryPresence.cloud(online: nil, verified: true, fetchedAt: now, now: now), .unknown)
        XCTAssertEqual(MobileInventoryPresence.cloud(online: true, verified: true, fetchedAt: nil, now: now), .unknown)
        for online in [true, false] {
            XCTAssertEqual(MobileInventoryPresence.cloud(online: online, verified: true, fetchedAt: now.addingTimeInterval(-60), now: now), .unknown)
        }
        XCTAssertTrue(MobileInventoryPresence.unknown.muted)
        XCTAssertTrue(MobileInventoryPresence.offline.muted)
        XCTAssertLessThan(MobileInventoryPresence.online.rank, MobileInventoryPresence.offline.rank)
    }
}

extension MobileTests {
    func testGalleryPlaybackPreservesFullReplayForMixedAndUnknownRecordings() {
        XCTAssertEqual(MobileRecordingLoadMode.playback(for: .audio), .audio)
        for kind: MobileRecordingKind? in [nil, .unknown, .mixed, .rosbag, .video, .images, .dataset] {
            XCTAssertEqual(MobileRecordingLoadMode.playback(for: kind), .full)
        }
    }
}
