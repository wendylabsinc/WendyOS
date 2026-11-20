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
    func testRestartWaitsForANewFinalHealthResult() throws {
        let json = #"{"status":{"hasResult":true,"outcome":"OUTCOME_ROLLED_BACK","createdAtUnix":"12","finalizedAtUnix":"14"},"install":{"active":false,"phase":"Installed","percent":100,"rebootRequired":true,"error":"","baseline":12}}"#
        var value = try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(json.utf8))
        XCTAssertFalse(value.status.hasFinalResult(after: 12))
        value.status.createdAtUnix = "13"
        XCTAssertTrue(value.status.hasFinalResult(after: 12))
        value.status.finalizedAtUnix = "0"
        XCTAssertFalse(value.status.hasFinalResult(after: 12))
    }
    func testRestartIsOfferedOnlyForAnInstalledUpdate() throws {
        let json = #"{"status":{"createdAtUnix":"12","engineStatus":{"connector":"tegrauefi","pending":{"phase":"installed"}}},"install":{"active":false,"phase":"","percent":-1,"rebootRequired":false,"error":"","baseline":0}}"#
        var value = try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(json.utf8))
        XCTAssertTrue(value.awaitsRestart)
        value.install.active = true
        XCTAssertFalse(value.awaitsRestart)
        value.install.active = false
        value.status.engineStatus?.pending?.phase = "failed"
        value.install.rebootRequired = true; value.install.baseline = 12
        XCTAssertFalse(value.awaitsRestart, "The backend refuses any other pending phase")
        value.status.engineStatus?.pending = nil
        XCTAssertTrue(value.awaitsRestart)
        value.status.createdAtUnix = "13"
        XCTAssertFalse(value.awaitsRestart, "A newer result means the restart already happened")
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
