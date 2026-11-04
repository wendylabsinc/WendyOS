import XCTest
@testable import WendyDesktop

final class DataVisualizerTimelineTests: XCTestCase {
    func testZoomPreservesAnchorAndSignedPreroll() {
        let episode = DataTimelineViewport(start: -10, end: 30)
        let zoomed = episode.zoomed(by: 2, anchor: 0.25, within: episode)
        XCTAssertEqual(zoomed.start, -5, accuracy: 0.000001)
        XCTAssertEqual(zoomed.end, 15, accuracy: 0.000001)
        XCTAssertEqual(zoomed.time(at: 0.25), episode.time(at: 0.25), accuracy: 0.000001)
    }

    func testPanPreservesScaleAndClampsAtBothEpisodeEdges() {
        let episode = DataTimelineViewport(start: -10, end: 30)
        let visible = DataTimelineViewport(start: 0, end: 10)
        XCTAssertEqual(visible.panned(by: -100, within: episode), DataTimelineViewport(start: -10, end: 0))
        XCTAssertEqual(visible.panned(by: 100, within: episode), DataTimelineViewport(start: 20, end: 30))
        XCTAssertEqual(visible.panned(by: 3, within: episode), DataTimelineViewport(start: 3, end: 13))
    }

    func testZoomLimitsFitAndRejectsInvalidInput() {
        let episode = DataTimelineViewport(start: -10, end: 30)
        let visible = DataTimelineViewport(start: 0, end: 10)
        XCTAssertEqual(visible.zoomed(by: 0.001, anchor: 0.5, within: episode), episode)
        XCTAssertEqual(visible.zoomed(by: 0, anchor: 0.5, within: episode), visible)
        XCTAssertEqual(visible.zoomed(by: .infinity, anchor: 0.5, within: episode), visible)
        XCTAssertEqual(visible.panned(by: .nan, within: episode), visible)
        XCTAssertEqual(visible.zoomed(by: 1e10, anchor: 0.5, within: episode).span, 0.001, accuracy: 0.000001)
    }

    func testCursorRevealDoesNotPanWhenCursorIsAlreadyVisible() {
        let episode = DataTimelineViewport(start: -10, end: 30)
        let visible = DataTimelineViewport(start: 0, end: 10)
        XCTAssertEqual(visible.revealing(5, within: episode), visible)
        let shifted = visible.revealing(20, within: episode)
        XCTAssertEqual(shifted.span, visible.span, accuracy: 0.000001)
        XCTAssertTrue(shifted.start <= 20 && shifted.end >= 20)
        XCTAssertEqual(visible.time(at: -1), visible.start)
        XCTAssertEqual(visible.time(at: 2), visible.end)
    }

    func testTickSpacingKeepsReadableIntervalsAcrossZoomLevels() {
        XCTAssertEqual(DataTimelineViewport.tickSpacing(span: 40, width: 720), 5)
        XCTAssertEqual(DataTimelineViewport.tickSpacing(span: 0.04, width: 720), 0.005, accuracy: 0.0000001)
        XCTAssertEqual(DataTimelineViewport.tickSpacing(span: 1000, width: 100), 1000)
    }
}

final class DataVisualizerPerformanceTests: XCTestCase {
    func testBinarySearchHandlesSignedTimesDuplicatesAndBounds() {
        let times = [-2.0, -1, -1, 0, 4]
        XCTAssertNil(DataVisualSearch.latest(times, at: -3, time: { $0 }))
        XCTAssertEqual(DataVisualSearch.upperBound(times, at: -1, time: { $0 }), 3)
        XCTAssertEqual(DataVisualSearch.latest(times, at: 2, time: { $0 }), 0)
        XCTAssertEqual(DataVisualSearch.latest(times, at: 9, time: { $0 }), 4)
        XCTAssertEqual(DataVisualSearch.upperBound([Double](), at: 0, time: { $0 }), 0)
    }

    func testLargeTrackLookupUsesLogarithmicComparisons() {
        let samples = Array(0..<100_000)
        var comparisons = 0
        let found = DataVisualSearch.latest(samples, at: 54_321.5) { value in
            comparisons += 1
            return Double(value)
        }
        XCTAssertEqual(found, 54_321)
        XCTAssertLessThanOrEqual(comparisons, 17)
    }

    func testSeekBackpressureKeepsOnlyLatestTargetAndFinalExactFrame() {
        var state = DataVisualSeekState()
        state.request(0)
        XCTAssertEqual(state.begin(), 0)
        for i in 1...1000 { state.request(Double(i)) }
        XCTAssertNil(state.begin(), "A running decode must not be canceled by each mouse event")
        state.finish(success: true)
        XCTAssertEqual(state.begin(), 1000, "Intermediate drag targets must not accumulate")
        state.request(2.345678)
        state.finish(success: true)
        XCTAssertEqual(state.begin(), 2.345678)
        state.finish(success: true)
        XCTAssertNil(state.begin(), "The final frame must not be decoded repeatedly")
        state.request(.nan)
        XCTAssertNil(state.begin())
    }

    func testChannelIndexRetainsAcquisitionTimesAndBoundsEventWindow() {
        let samples = (0..<200).map { i in
            DataVisualSample(id: i, track: i.isMultiple(of: 2) ? "events" : "numeric",
                             label: "sample", detail: "", unit: "", timestamp: Int64(i - 100) * 1_000_000_000,
                             value: i.isMultiple(of: 2) ? nil : Double(i), image: nil)
        }
        let tracks = [DataVisualTrack(id: "events", name: "Events", kind: "event", detail: ""),
                      DataVisualTrack(id: "numeric", name: "Numeric", kind: "numeric", detail: "")]
        let data = DataVisualSnapshot(clock: "CLOCK_BOOTTIME", bootID: "boot", warnings: "", start: -100, end: 100,
                                      tracks: tracks, samples: samples, videos: [], mediaFiles: DataVisualMediaFiles())
        let index = DataVisualIndex(data)
        XCTAssertEqual(index.channels["numeric"]?.count, 100)
        XCTAssertEqual(index.latest(track: "numeric", at: -50)?.timestamp, -51_000_000_000)
        XCTAssertNil(index.latest(track: "numeric", at: -101))
        XCTAssertEqual(index.events(through: -100).count, 1)
        XCTAssertEqual(index.events(through: 100).count, 80)
        XCTAssertEqual(index.events(through: 100).first?.timestamp, -60_000_000_000)
        XCTAssertEqual(index.numericRanges["numeric"], 1...199)
    }

    @MainActor
    func testMouseBurstCoalescesAndMouseUpFlushesFinalPosition() throws {
        let view = DataTimelineInputView(frame: CGRect(x: 0, y: 0, width: 100, height: 30))
        view.windowRange = DataTimelineViewport(start: -10, end: 10)
        let playhead = DataTimelinePlayhead()
        let ruler = DataTimelineInputView(frame: CGRect(x: 0, y: 0, width: 200, height: 30))
        let otherTrack = DataTimelineInputView(frame: CGRect(x: 0, y: 0, width: 100, height: 16))
        view.playhead = playhead
        for subscriber in [view, ruler, otherTrack] { playhead.attach(subscriber) }
        var received: [Double] = []
        view.seek = { received.append($0) }
        func event(_ type: NSEvent.EventType, _ x: Double) throws -> NSEvent {
            try XCTUnwrap(NSEvent.mouseEvent(with: type, location: CGPoint(x: x, y: 15), modifierFlags: [],
                                            timestamp: 0, windowNumber: 0, context: nil, eventNumber: 0, clickCount: 1, pressure: 1))
        }
        view.mouseDown(with: try event(.leftMouseDown, 10))
        for x in 11...90 { view.mouseDragged(with: try event(.leftMouseDragged, Double(x))) }
        XCTAssertTrue(received.isEmpty)
        XCTAssertTrue(view.hasPendingSeek)
        // All playheads follow the pointer before any expensive panel refresh.
        XCTAssertEqual(view.cursor, 8)
        XCTAssertEqual(ruler.cursor, 8)
        XCTAssertEqual(otherTrack.cursor, 8)
        // A delayed SwiftUI update must not pull the pointer backwards.
        playhead.synchronize(-5)
        XCTAssertEqual(ruler.cursor, 8)
        view.mouseUp(with: try event(.leftMouseUp, 95))
        XCTAssertEqual(received, [9])
        XCTAssertFalse(view.hasPendingSeek)
        XCTAssertEqual(ruler.cursor, 9)
        XCTAssertEqual(otherTrack.cursor, 9)
        // Playback and keyboard updates take over after the gesture ends.
        playhead.synchronize(4)
        XCTAssertEqual(ruler.cursor, 4)
    }

    @MainActor
    func testDetachedDragDoesNotBlockPlaybackUpdates() {
        let playhead = DataTimelinePlayhead()
        let dragged = DataTimelineInputView(frame: .zero)
        let ruler = DataTimelineInputView(frame: .zero)
        playhead.attach(dragged)
        playhead.attach(ruler)
        playhead.drag(to: 3, owner: dragged)
        playhead.synchronize(1)
        XCTAssertEqual(ruler.cursor, 3)
        playhead.detach(dragged)
        playhead.synchronize(7)
        XCTAssertEqual(ruler.cursor, 7)
    }
}
