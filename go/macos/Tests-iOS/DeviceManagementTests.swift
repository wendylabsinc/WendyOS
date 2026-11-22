import XCTest
@testable import WendyMobile

final class DeviceManagementTests: XCTestCase {
    func testDownloadProgressUsesReportedBytesOnly() {
        var update = DeviceUpdateSnapshot(installed: "old", latest: "new", phase: "downloading", detail: "", available: true)
        XCTAssertNil(update.downloadFraction, "An unknown total must remain indeterminate")
        update.downloaded = 25; update.total = 100
        XCTAssertEqual(update.downloadFraction, 0.25)
        update.downloaded = 110
        XCTAssertEqual(update.downloadFraction, 1)
        update.downloaded = -1
        XCTAssertNil(update.downloadFraction)
    }
    func testUpdateBusyDoesNotTreatFailedAsRunning() {
        for phase in ["downloading", "installing", "restarting"] {
            XCTAssertTrue(DeviceUpdateSnapshot(installed: "old", latest: "new", phase: phase, detail: "", available: true).busy)
        }
        for phase in ["failed", "current", "available", ""] {
            XCTAssertFalse(DeviceUpdateSnapshot(installed: "old", latest: "new", phase: phase, detail: "", available: true).busy)
        }
    }
}
