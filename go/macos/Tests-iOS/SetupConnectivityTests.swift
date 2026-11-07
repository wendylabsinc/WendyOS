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

final class WiFiManagementValidationTests: XCTestCase {
    func testJoinCredentialsAndSavedProfileReuse() {
        XCTAssertNil(WiFiFormValidation.issue(ssid: "Saved", password: "", security: 4, saved: true))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: "New", password: "", security: 4, saved: false))
        XCTAssertNil(WiFiFormValidation.issue(ssid: "Open", password: "", security: 1, saved: false))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: String(repeating: "é", count: 17), password: "password", security: 4, saved: false))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: "New", password: "short", security: 4, saved: false))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: "New", password: String(repeating: "x", count: 64), security: 4, saved: false))
        XCTAssertNil(WiFiFormValidation.issue(ssid: "New", password: String(repeating: "a", count: 64), security: 4, saved: false))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: "New", password: "password", security: 6, saved: false))
        XCTAssertNotNil(WiFiFormValidation.issue(ssid: "New", password: "nul\0password", security: 4, saved: false))
    }
}
