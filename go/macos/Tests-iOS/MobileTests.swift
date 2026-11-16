import XCTest
import ARKit
import RoomPlan
import AVFoundation
import SwiftUI

@testable import WendyMobile

final class MobileTests: XCTestCase {
    @MainActor
    func testOverflowMenuKeepsFullHitArea() async throws {
        let scene = try XCTUnwrap(UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.first)
        let previousWindow = scene.windows.first(where: \.isKeyWindow)
        let window = UIWindow(windowScene: scene)
        let host = UIHostingController(rootView: OverflowMenu { Button("Inspect") {} })
        window.rootViewController = host
        window.makeKeyAndVisible()
        defer { window.isHidden = true; previousWindow?.makeKeyAndVisible() }
        try await Task.sleep(for: .milliseconds(100))
        host.view.layoutIfNeeded()
        func buttons(_ view: UIView) -> [UIButton] {
            (view as? UIButton).map { [$0] } ?? view.subviews.flatMap(buttons)
        }
        let button = try XCTUnwrap(buttons(host.view).first)
        XCTAssertGreaterThanOrEqual(button.bounds.width, 44)
        XCTAssertGreaterThanOrEqual(button.bounds.height, 44)
        for x in [CGFloat(1), button.bounds.width - 1] {
            for y in [CGFloat(1), button.bounds.height - 1] {
                XCTAssertTrue(button.point(inside: CGPoint(x: x, y: y), with: nil))
            }
        }
    }

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
                    case .loginError(let message): throw WendyError(message: message)
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

    private func initializeIsolatedSandbox() async throws {
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
