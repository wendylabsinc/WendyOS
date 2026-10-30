import XCTest

@testable import WendyMobile

final class MobileTests: XCTestCase {
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
        try await client.initializeMobile()
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
        if !resumed.lite { _ = try await client.apps() }
        try await client.disconnect()
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
