import XCTest
@testable import WendyMobile

final class SetupConnectivityTests: XCTestCase {
    func testNetworkRequiresObservationOrExplicitEthernetChoice() {
        var flow = NearbySetupFlow()
        flow.continueFromNetwork(connected: true)
        XCTAssertEqual(flow.step, .device)
        flow.choseDevice()
        flow.continueFromNetwork(connected: false)
        XCTAssertEqual(flow.step, .network)
        flow.continueFromNetwork(connected: false, useEthernet: true)
        XCTAssertEqual(flow.step, .organization)
        flow.backToNetwork()
        XCTAssertEqual(flow.step, .network)
        flow.continueFromNetwork(connected: true)
        XCTAssertEqual(flow.step, .organization)
    }

    func testWiFiValidationUsesProtocolBytesAndRejectsBlankSSID() {
        XCTAssertFalse(NearbySetupFlow.validWiFi(ssid: "  ", password: ""))
        XCTAssertTrue(NearbySetupFlow.validWiFi(ssid: String(repeating: "é", count: 16), password: ""))
        XCTAssertFalse(NearbySetupFlow.validWiFi(ssid: String(repeating: "é", count: 17), password: ""))
        XCTAssertTrue(NearbySetupFlow.validWiFi(ssid: "Open network", password: ""))
        XCTAssertTrue(NearbySetupFlow.validWiFi(ssid: "Network", password: String(repeating: "a", count: 64)))
        XCTAssertFalse(NearbySetupFlow.validWiFi(ssid: "Network", password: String(repeating: "a", count: 65)))
    }
}
