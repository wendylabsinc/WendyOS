import XCTest
@testable import WendyDesktop

final class NearbySetupPromptTests: XCTestCase {
    func testNearestDevicePromptsOnceAndUnknownSignalsDoNotPrompt() {
        var history = NearbySetupPromptHistory()
        let devices = [
            NearbySetupDevice(id: "far", name: "Far", rssi: -85),
            NearbySetupDevice(id: "unknown", name: "Unknown", rssi: 0),
            NearbySetupDevice(id: "near", name: "Near", rssi: -40),
            NearbySetupDevice(id: "edge", name: "Edge", rssi: -80),
        ]
        XCTAssertEqual(history.next(devices)?.id, "near")
        XCTAssertEqual(history.next(devices)?.id, "edge")
        XCTAssertNil(history.next(devices))
        XCTAssertNil(history.next([]))
        XCTAssertNil(history.next([devices[2]]), "Returning or dismissed devices must not repeatedly interrupt setup")
    }

    func testNamesDoNotCollapseDifferentDevices() {
        var history = NearbySetupPromptHistory()
        let devices = [NearbySetupDevice(id: "b", name: "Wendy", rssi: -40), NearbySetupDevice(id: "a", name: "Wendy", rssi: -40)]
        XCTAssertEqual(history.next(devices)?.id, "a")
        XCTAssertEqual(history.next(devices)?.id, "b")
    }
}

final class NearbySetupObservationTests: XCTestCase {
    private func next(_ state: NearbySetupSnapshot, selected: Bool = true, wifi: Bool = false,
                      statusAge: TimeInterval = 30, scanAge: TimeInterval = 30) -> NearbySetupObservation {
        state.observation(hasSelection: selected, wifiExpanded: wifi, statusAge: statusAge, scanAge: scanAge)
    }

    func testDiscoveryAndConnectionRecoverWithoutAnAction() {
        XCTAssertEqual(next(NearbySetupSnapshot(), selected: false), .discover)
        XCTAssertEqual(next(NearbySetupSnapshot()), .reconnect)
        XCTAssertEqual(next(NearbySetupSnapshot(connected: true)), .status)
    }

    func testLostEnrollmentReplyKeepsCheckingCloudWithoutReconnectingOrReenrolling() {
        var state = NearbySetupSnapshot(attempted: true, pending: true)
        XCTAssertEqual(next(state, statusAge: 600), .checkEnrollment)
        state.enrolled = true
        XCTAssertEqual(next(state), .checkEnrollment)
        state.completed = true
        XCTAssertEqual(next(state), .idle)
        XCTAssertEqual(next(NearbySetupSnapshot(attempted: true)), .idle,
                       "An ambiguous enrollment without saved credentials must never be repeated")
    }

    func testPendingEnrollmentKeepsWiFiAndKeepaliveAvailable() {
        var state = NearbySetupSnapshot(connected: true, attempted: true, pending: true)
        XCTAssertEqual(next(state), .status)
        XCTAssertEqual(next(state, wifi: true, statusAge: 1), .scanWiFi)
        XCTAssertEqual(next(state, wifi: true, statusAge: 1, scanAge: 1), .checkEnrollment)
        state.connected = false
        XCTAssertEqual(next(state, wifi: true), .reconnect)
        XCTAssertEqual(next(state, wifi: true, statusAge: 1), .checkEnrollment)
        XCTAssertEqual(next(state), .checkEnrollment)
    }

    func testFailedOrExpiredEnrollmentDoesNotKeepPollingCloud() {
        var state = NearbySetupSnapshot(attempted: true, canResume: true, failure: "Could not enroll")
        XCTAssertEqual(next(state), .reconnect)
        state.connected = true
        XCTAssertEqual(next(state, statusAge: 1, scanAge: 1), .idle)
        state.canResume = false
        XCTAssertEqual(next(state, statusAge: 1, scanAge: 1), .idle)
    }

    func testWiFiDiscoveryOnlyRunsWhileVisibleAndThrottlesReads() {
        let state = NearbySetupSnapshot(connected: true)
        XCTAssertEqual(next(state, statusAge: 1), .idle)
        XCTAssertEqual(next(state, wifi: true, statusAge: 1), .scanWiFi)
        XCTAssertEqual(next(state, wifi: true, statusAge: 1, scanAge: 1), .idle)
        XCTAssertEqual(next(NearbySetupSnapshot(enrolled: true), wifi: true), .idle)
    }
}
