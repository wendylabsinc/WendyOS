import XCTest
@testable import WendyDesktop

final class NetworkTrafficTests: XCTestCase {
    func testSnapshotCopiesCountersAndDistinguishesZeroFromMissingRate() {
        let pointer = calloc(1, MemoryLayout<WDNetworkTrafficResult>.stride)!.assumingMemoryBound(to: WDNetworkTrafficResult.self)
        let rows = calloc(2, MemoryLayout<WDNetworkTrafficRow>.stride)!.assumingMemoryBound(to: WDNetworkTrafficRow.self)
        pointer.pointee.rows = rows
        pointer.pointee.count = 2
        rows[0].name = strdup("eth0")
        rows[0].index = 3
        rows[0].received_bytes = UInt64.max - 1
        rows[0].receive_rate = WDOptionalDouble(available: 1, value: 0)
        rows[1].name = strdup("Wendy agent service")
        rows[1].agent = 1
        rows[1].sent_bytes = 3000
        let snapshot = NetworkTrafficSnapshot(pointer.pointee)
        WDNetworkTrafficFree(pointer)
        XCTAssertEqual(snapshot.rows[0].name, "eth0")
        XCTAssertEqual(snapshot.rows[0].received, UInt64.max - 1)
        XCTAssertEqual(snapshot.rows[0].receiveRate, 0)
        XCTAssertNil(snapshot.rows[0].sendRate)
        XCTAssertEqual(snapshot.rows[1].id, "agent")
        XCTAssertEqual(snapshot.rows[1].sent, 3000)
    }

    func testOlderAgentFailureHasSpecificMessageAndKeepsCause() {
        let raw = "agent does not support network traffic statistics: rpc error: code = Unimplemented"
        let failure = PresentedFailure(raw)
        XCTAssertTrue(failure.message.contains("does not report network traffic"))
        XCTAssertEqual(failure.details, raw)
    }
}
