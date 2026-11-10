import XCTest
@testable import WendyMobile

final class MobileRecordingTests: XCTestCase {
    func testFilteredExportWaitsForMissingLabelsAndNeverIncludesAHiddenSelection() {
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        func episode(_ id: String, _ reason: String?, loaded: Bool = true) -> MobileRecordingEpisode {
            MobileRecordingEpisode(id: id, title: id, started: nil, bytes: 100, duration: nil, trigger: reason,
                                   kind: .unknown, synced: nil, topics: [], detailsLoaded: loaded)
        }
        let motion = episode("first", "Motion"), other = episode("second", "Manual")
        let partial = MobileRecordingLibrary(device: device, model: nil,
                                            episodes: [motion, other, episode("third", nil, loaded: false)], warning: nil)
        XCTAssertEqual(partial.exportTargets(trigger: nil, selectedID: nil).count, 3)
        XCTAssertTrue(partial.exportTargets(trigger: "Motion", selectedID: nil).isEmpty)
        XCTAssertEqual(partial.exportTargets(trigger: "Motion", selectedID: motion.id).map(\.id), ["first"])
        XCTAssertTrue(partial.exportTargets(trigger: "Motion", selectedID: other.id).isEmpty)

        let complete = MobileRecordingLibrary(device: device, model: nil,
                                              episodes: [motion, other, episode("third", "Motion")], warning: nil)
        XCTAssertNil(complete.labelCoverage)
        XCTAssertEqual(complete.labels.map(\.name), ["Motion", "Manual"])
        XCTAssertEqual(complete.labels.map(\.count), [2, 1])
        XCTAssertEqual(complete.exportTargets(trigger: "Motion", selectedID: nil).map(\.id), ["first", "third"])
    }

    func testKindAndLabelFiltersIntersectAndExportCannotIncludeHiddenRows() {
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        func row(_ id: String, _ kind: MobileRecordingKind, _ trigger: String, loaded: Bool = true) -> MobileRecordingEpisode {
            MobileRecordingEpisode(id: id, title: id, started: nil, bytes: 100, duration: nil,
                trigger: trigger, kind: kind, synced: nil, topics: [], detailsLoaded: loaded)
        }
        let rows = [row("video", .video, "Person"), row("audio", .audio, "Person"), row("mix", .mixed, "Manual")]
        let library = MobileRecordingLibrary(device: device, model: nil, episodes: rows, warning: nil)
        XCTAssertEqual(library.episodes(matching: "Person", kind: .audio).map(\.id), ["audio"])
        XCTAssertEqual(library.episodes(matching: nil, kind: .mixed).map(\.id), ["mix"])
        XCTAssertTrue(library.episodes(matching: "Person", kind: .rosbag).isEmpty)
        XCTAssertEqual(library.exportTargets(trigger: nil, selectedID: nil, kind: .audio).map(\.id), ["audio"])
        XCTAssertTrue(library.exportTargets(trigger: nil, selectedID: "video", kind: .audio).isEmpty)
        let partial = MobileRecordingLibrary(device: device, model: nil,
            episodes: rows + [row("pending", .unknown, "", loaded: false)], warning: nil)
        XCTAssertTrue(partial.exportTargets(trigger: nil, selectedID: nil, kind: .audio).isEmpty)
        XCTAssertEqual(partial.exportTargets(trigger: nil, selectedID: "audio", kind: .audio).map(\.id), ["audio"])
        XCTAssertEqual(MobileRecordingKind(agentKind: "mixed"), .mixed)
    }

    func testOnlyMatchingTenantAndDeviceIDsShareARecordingCard() {
        func route(_ id: String, _ transport: DeviceTransport) -> Device {
            Device(id: id, name: "Same name", selector: id, transport: transport, address: "same-address")
        }
        let nearby = route("spiffe://wendy.sh/tenant/first/device/asset-id", .lan)
        let cloud = route("cloud://api.wendy.dev:443/tenant/first/asset/asset-id", .cloud)
        let otherOrg = route("cloud://api.wendy.dev:443/tenant/second/asset/asset-id", .cloud)
        let unknown = route("speaker.local:50051", .lan)
        XCTAssertEqual(recordingDeviceID(nearby), recordingDeviceID(cloud))
        XCTAssertNotEqual(recordingDeviceID(nearby), recordingDeviceID(otherOrg))
        XCTAssertNotEqual(recordingDeviceID(unknown), recordingDeviceID(cloud))
    }
}

private actor RecordingSpeedFixture {
    var indexes = 0
    var details = 0
    var fail = false
    var bytes: Int64 = 100
    var delay: Duration = .zero
    func configure(fail: Bool = false, bytes: Int64 = 100, delay: Duration = .zero) {
        self.fail = fail; self.bytes = bytes; self.delay = delay
    }
    func counts() -> (Int, Int) { (indexes, details) }
    func fetch(_ target: MobileRecordingTarget) async -> RecordingRead {
        indexes += 1
        try? await Task.sleep(for: delay)
        if fail { return RecordingRead(target: target, problem: "Offline") }
        let episode = MobileRecordingEpisode(id: "episode", title: "Clip", started: nil, bytes: bytes,
            duration: nil, trigger: nil, kind: .unknown, synced: nil, topics: [])
        return RecordingRead(target: target, library: MobileRecordingLibrary(device: target.routes[0], model: nil,
            episodes: [episode], warning: nil))
    }
    func loadDetails(_ device: Device, _ episodes: [MobileRecordingEpisode]) -> [MobileRecordingEpisode] {
        details += episodes.count
        return episodes.map { row in
            MobileRecordingEpisode(id: row.id, title: row.title, started: row.started, bytes: row.bytes,
                duration: 10, trigger: "Person", kind: .video, synced: nil, topics: [], detailsLoaded: true)
        }
    }
}

final class RecordingSpeedTests: XCTestCase {
    private var target: MobileRecordingTarget {
        MobileRecordingTarget(id: "board", routes: [Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")])
    }

    @MainActor func testFreshCatalogAvoidsRequestsAcrossScreensAndRefreshReusesDetails() async {
        let fixture = RecordingSpeedFixture()
        var clock = Date(timeIntervalSince1970: 100)
        let catalog = RecordingCatalog(now: { clock }, fetch: { await fixture.fetch($0) },
                                       details: { await fixture.loadDetails($0, $1) })
        await catalog.load([target], refresh: 0, priority: nil)
        await catalog.load([target], refresh: 0, priority: nil, consumer: "activity")
        await catalog.load([target], refresh: 0, priority: "board")
        var counts = await fixture.counts()
        XCTAssertEqual(counts.0, 1, "Tab visits within 30 seconds must not repeat the index request")
        XCTAssertEqual(counts.1, 1)
        await catalog.load([target], refresh: 1, priority: nil)
        counts = await fixture.counts()
        XCTAssertEqual(counts.0, 2, "Pull to refresh must bypass freshness")
        XCTAssertEqual(counts.1, 1, "Unchanged finalized episodes must reuse cached tags")
        await catalog.load([target], refresh: 0, priority: nil, consumer: "activity")
        counts = await fixture.counts()
        XCTAssertEqual(counts.0, 2, "Activity's refresh counter must not invalidate Data's refresh")
        clock += 31
        await fixture.configure(bytes: 200)
        await catalog.load([target], refresh: 1, priority: nil)
        counts = await fixture.counts()
        XCTAssertEqual(counts.0, 3)
        XCTAssertEqual(counts.1, 2, "Changed index metadata invalidates the cached episode")
        XCTAssertEqual(catalog.reads["board"]?.library?.episodes.first?.bytes, 200)
    }

    @MainActor func testFailedRefreshRetainsLabeledSavedDataAndScopeDoesNotShareCache() async {
        let fixture = RecordingSpeedFixture()
        let catalog = RecordingCatalog(fetch: { await fixture.fetch($0) }, details: { await fixture.loadDetails($0, $1) })
        await catalog.load([target], refresh: 0, priority: nil)
        await fixture.configure(fail: true)
        await catalog.load([target], refresh: 1, priority: nil)
        XCTAssertEqual(catalog.reads["board"]?.library?.episodes.first?.trigger, "Person")
        XCTAssertTrue(catalog.reads["board"]?.problem?.contains("Showing saved recordings") == true)
        let otherScope = RecordingCatalog(fetch: { await fixture.fetch($0) }, details: { await fixture.loadDetails($0, $1) })
        XCTAssertTrue(otherScope.reads.isEmpty)
        await otherScope.load([target], refresh: 0, priority: nil)
        XCTAssertNil(otherScope.reads["board"]?.library)
        await catalog.load([], refresh: 1, priority: nil)
        XCTAssertTrue(catalog.reads.isEmpty, "Removed targets must not remain accessible from cached inventory")
    }

    @MainActor func testCanceledIndexCannotPublishIntoNewGeneration() async {
        let fixture = RecordingSpeedFixture()
        await fixture.configure(delay: .milliseconds(100))
        let catalog = RecordingCatalog(fetch: { await fixture.fetch($0) }, details: { await fixture.loadDetails($0, $1) })
        let target = target
        await withTaskGroup(of: Void.self) { group in
            group.addTask { await catalog.load([target], refresh: 0, priority: nil) }
            while await fixture.counts().0 == 0 { await Task.yield() }
            group.cancelAll()
            await catalog.load([], refresh: 0, priority: nil)
        }
        XCTAssertTrue(catalog.reads.isEmpty)
        XCTAssertFalse(catalog.loading)
    }
}
