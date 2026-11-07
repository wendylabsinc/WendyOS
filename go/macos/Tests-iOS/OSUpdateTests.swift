import XCTest
@testable import WendyMobile

final class OSUpdateTests: XCTestCase {
    func testCommitMustBeNewerAndMatchInstalledVersion() throws {
        let json = #"{"status":{"hasResult":true,"outcome":"OUTCOME_COMMITTED","createdAtUnix":"12","newOsVersion":"2.0"},"install":{"active":false,"phase":"Installed","percent":100,"rebootRequired":true,"error":"","baseline":12}}"#
        var value = try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(json.utf8))
        XCTAssertFalse(value.status.verifies(value.install, installedVersion: "2.0"))
        value.status.createdAtUnix = "13"
        XCTAssertTrue(value.status.verifies(value.install, installedVersion: "2.0"))
        XCTAssertFalse(value.status.verifies(value.install, installedVersion: "1.0"))
        value.status.outcome = "OUTCOME_ROLLED_BACK"
        XCTAssertFalse(value.status.verifies(value.install, installedVersion: "2.0"))
        XCTAssertEqual(value.status.title, "Rollback awaiting verification")
        value.status.finalizedAtUnix = "14"
        XCTAssertEqual(value.status.title, "Previous OS restored")
    }
    func testUnreportedOrInvalidProgressRemainsIndeterminate() throws {
        let json = #"{"status":{},"install":{"active":true,"phase":"Starting","percent":-1,"rebootRequired":false,"error":"","baseline":0}}"#
        var value = try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(json.utf8))
        XCTAssertNil(value.install.fraction)
        value.install.percent = 20
        XCTAssertEqual(value.install.fraction, 0.2)
        value.install.percent = 101
        XCTAssertNil(value.install.fraction)
        XCTAssertFalse(value.status.verifies(value.install, installedVersion: "2.0"))
    }
}
