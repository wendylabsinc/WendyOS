import XCTest
@testable import WendyDesktop

@MainActor
final class CampaignEventTests: XCTestCase {
    private func clip(_ id: String, at seconds: Double, state: String = "complete") -> CampaignClip {
        CampaignClip(id: id, campaign: "human-detected", state: state, reason: "Person detected", started: Date(timeIntervalSince1970: seconds), bytes: 100, duration: 5)
    }

    func testLatestThreeAreDeduplicatedAndSortedByCaptureTime() {
        let suite = "CampaignEventTests.\(UUID())"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = CampaignEventStore(deviceID: "thor", defaults: defaults)
        store.receive(CampaignClipsSnapshot(clips: [clip("a", at: 1), clip("d", at: 4, state: "recording"), clip("b", at: 2), clip("c", at: 3), clip("d", at: 4)], warning: "Partial history"))
        XCTAssertEqual(store.latest.map(\.id), ["d", "c", "b"])
        XCTAssertEqual(store.clips.count, 4)
        XCTAssertEqual(store.latest.first?.state, "complete")
        XCTAssertEqual(store.warning, "Partial history")
        XCTAssertTrue(store.hasUnread)
    }

    func testReadReceiptsSurviveReconnectAndAreIsolatedByDevice() {
        let suite = "CampaignEventTests.\(UUID())"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let snapshot = CampaignClipsSnapshot(clips: [clip("same-id", at: 10)], warning: "")
        let store = CampaignEventStore(deviceID: "thor", defaults: defaults)
        store.receive(snapshot)
        store.markRead(["same-id"])
        XCTAssertFalse(store.hasUnread)
        let reconnected = CampaignEventStore(deviceID: "thor", defaults: defaults)
        reconnected.receive(snapshot)
        XCTAssertFalse(reconnected.hasUnread)
        let other = CampaignEventStore(deviceID: "orin", defaults: defaults)
        other.receive(snapshot)
        XCTAssertTrue(other.hasUnread)
    }

    func testDelayedOlderEventStillRaisesUnreadButClipUpdatesDoNot() {
        let suite = "CampaignEventTests.\(UUID())"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = CampaignEventStore(deviceID: "thor", defaults: defaults)
        store.receive(CampaignClipsSnapshot(clips: [clip("new", at: 10, state: "recording")], warning: ""))
        store.markRead(["new"])
        store.receive(CampaignClipsSnapshot(clips: [clip("new", at: 10)], warning: ""))
        XCTAssertFalse(store.hasUnread)
        store.receive(CampaignClipsSnapshot(clips: [clip("new", at: 10), clip("delayed", at: 5)], warning: ""))
        XCTAssertTrue(store.hasUnread)
        store.markRead(["delayed"])
        XCTAssertFalse(store.hasUnread)
    }
}
