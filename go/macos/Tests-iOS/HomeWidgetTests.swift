import XCTest
@testable import WendyMobile

final class HomeWidgetTests: XCTestCase {
    @MainActor func testLayoutsPersistOrderAndStayIsolatedByAccountAndOrganization() {
        let name = "HomeWidgetTests." + UUID().uuidString
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        var favorite = HomeWidget(kind: .favorites)
        favorite.devices = [HomeWidgetDevice(device)]
        var gallery = HomeWidget(kind: .gallery)
        gallery.devices = favorite.devices; gallery.label = "Person"; gallery.dataKind = MobileRecordingKind.video.rawValue
        let store = HomeWidgetStore(defaults: defaults)
        store.load(account: "alice", organization: "one")
        store.save(favorite); store.save(gallery)
        store.move(from: IndexSet(integer: 1), to: 0)
        let reopened = HomeWidgetStore(defaults: defaults)
        reopened.load(account: "alice", organization: "one")
        XCTAssertEqual(reopened.items, [gallery, favorite])
        reopened.load(account: "alice", organization: "two")
        XCTAssertTrue(reopened.items.isEmpty)
        reopened.load(account: "bob", organization: "one")
        XCTAssertTrue(reopened.items.isEmpty)
        reopened.load(account: "alice", organization: "one")
        reopened.remove(gallery.id)
        XCTAssertEqual(reopened.items, [favorite])
        favorite.size = .compact; reopened.save(favorite)
        XCTAssertEqual(reopened.items, [favorite])
    }
    @MainActor func testIncompleteWidgetAndCorruptLayoutDoNotOverwriteSavedData() {
        let name = "HomeWidgetTests." + UUID().uuidString
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let key = HomeWidgetStore.storageKey(account: "alice", organization: "one")
        let store = HomeWidgetStore(defaults: defaults)
        store.load(account: "alice", organization: "one")
        store.save(HomeWidget(kind: .camera))
        XCTAssertTrue(store.items.isEmpty)
        let corrupt = Data("broken".utf8)
        defaults.set(corrupt, forKey: key)
        let reopened = HomeWidgetStore(defaults: defaults)
        reopened.load(account: "alice", organization: "one")
        XCTAssertNotNil(reopened.error)
        reopened.remove(UUID())
        XCTAssertEqual(defaults.data(forKey: key), corrupt)
        XCTAssertNotEqual(HomeWidgetStore.storageKey(account: "a/b", organization: "c"), HomeWidgetStore.storageKey(account: "a", organization: "b/c"))
    }
}

private actor HomeCatalogFixture {
    private var requests = 0
    func count() -> Int { requests }
    func fetch(_ target: MobileRecordingTarget) -> RecordingRead {
        requests += 1
        return RecordingRead(target: target, library: MobileRecordingLibrary(device: target.routes[0], model: nil, episodes: [], warning: nil))
    }
}
extension HomeWidgetTests {
    @MainActor func testHomeOnlyFetchesConfiguredDevicesWithoutEvictingCurrentFleetCache() async {
        func target(_ name: String) -> MobileRecordingTarget {
            MobileRecordingTarget(id: name, routes: [Device(id: name, name: name, selector: name, transport: .lan, address: "")])
        }
        let one = target("one"), two = target("two")
        let fixture = HomeCatalogFixture()
        let catalog = RecordingCatalog(fetch: { await fixture.fetch($0) }, details: { _, episodes in episodes })
        await catalog.load([one, two], refresh: 0, priority: nil)
        await catalog.load([one], refresh: 0, priority: nil, consumer: "home.widgets", retaining: [one, two])
        XCTAssertEqual(Set(catalog.reads.keys), ["one", "two"])
        let count = await fixture.count()
        XCTAssertEqual(count, 2)
        await catalog.load([], refresh: 0, priority: nil, consumer: "home.widgets", retaining: [one])
        XCTAssertEqual(Set(catalog.reads.keys), ["one"], "A removed device must be evicted even when there are no gallery widgets")
    }
}

extension HomeWidgetTests {
    func testDenseGridFillsHalfRowsWithoutOverlap() {
        let sizes: [HomeWidgetSize] = [.small, .medium, .large, .small, .small]
        let placements = HomeWidgetGridPlacement.pack(sizes)
        XCTAssertEqual(placements[0], .init(column: 0, row: 0, columns: 2, rows: 2))
        XCTAssertEqual(placements[3], .init(column: 2, row: 0, columns: 2, rows: 2))
        var occupied = Set<Int>()
        for p in placements {
            XCTAssertLessThanOrEqual(p.column + p.columns, 4)
            for r in p.row..<(p.row + p.rows) {
                for c in p.column..<(p.column + p.columns) {
                    XCTAssertTrue(occupied.insert(r * 4 + c).inserted)
                }
            }
        }
        XCTAssertTrue(HomeWidgetGridPlacement.pack(sizes, fullWidth: true).allSatisfy { $0.columns == 4 && $0.column == 0 })
    }
    @MainActor func testLegacySizesAndSectionsMigrateOnceWithoutLosingTargets() throws {
        let suite = "HomeWidgetMigration." + UUID().uuidString
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let device = Device(id: "board", name: "Board", selector: "board:50051", transport: .lan, address: "")
        var favorite = HomeWidget(kind: .favorites)
        favorite.devices = [HomeWidgetDevice(device)]
        favorite.size = .small
        let data = try JSONEncoder().encode([favorite])
        let legacy = String(decoding: data, as: UTF8.self).replacingOccurrences(of: "Small", with: "Compact")
        defaults.set(Data(legacy.utf8), forKey: HomeWidgetStore.storageKey(account: "a", organization: "one"))
        defaults.set("Happening now", forKey: "wendy.home.hidden")
        let store = HomeWidgetStore(defaults: defaults)
        store.load(account: "a", organization: "one")
        XCTAssertEqual(store.items.first, favorite)
        XCTAssertEqual(store.items.map(\.kind), [.favorites, .attention, .actions])
        let reopened = HomeWidgetStore(defaults: defaults)
        reopened.load(account: "a", organization: "one")
        XCTAssertEqual(reopened.items, store.items)
        reopened.load(account: "a", organization: "two")
        XCTAssertTrue(reopened.items.isEmpty)
        XCTAssertEqual(try JSONDecoder().decode(HomeWidgetSize.self, from: Data("\"Expanded\"".utf8)), .medium)
    }
    @MainActor func testBuiltInResizeAndGalleryAllDevices() {
        let suite = "HomeWidgetResize." + UUID().uuidString
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = HomeWidgetStore(defaults: defaults)
        store.load(account: "a", organization: "b")
        let actions = HomeWidget(kind: .actions)
        store.save(actions)
        store.resize(actions.id)
        XCTAssertEqual(store.items.first?.size, .small)
        store.resize(actions.id)
        XCTAssertEqual(store.items.first?.size, .medium)
        XCTAssertTrue(HomeWidget(kind: .gallery).valid)
        XCTAssertFalse(HomeWidget(kind: .camera).valid)
    }
}


extension HomeWidgetTests {
    @MainActor func testRecordingCacheSurvivesRouteChangesAndUnavailableDevice() async {
        let first = MobileRecordingTarget(id: "board", routes: [Device(id: "board", name: "Board", selector: "old:50051", transport: .lan, address: "")])
        let changed = MobileRecordingTarget(id: "board", routes: [Device(id: "board", name: "Board renamed", selector: "new:50051", transport: .lan, address: "")])
        let fixture = HomeCatalogFixture()
        let catalog = RecordingCatalog(fetch: { await fixture.fetch($0) }, details: { _, episodes in episodes })
        await catalog.load([first], refresh: 0, priority: nil)
        await catalog.load([], refresh: 0, priority: nil, retaining: [changed])
        XCTAssertNotNil(catalog.reads["board"]?.library, "An unavailable known device keeps saved recordings")
        await catalog.load([changed], refresh: 0, priority: nil, consumer: "home.widgets", retaining: [changed])
        let count = await fixture.count()
        XCTAssertEqual(count, 1, "Route/name changes must not invalidate fresh recording data")
    }
}
