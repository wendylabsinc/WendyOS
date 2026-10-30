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
