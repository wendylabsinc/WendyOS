import XCTest
import Combine
import NetworkExtension
@testable import WendyDesktop

final class VPNInternetTests: XCTestCase {
    @MainActor
    func testRepeatedSystemStatusDoesNotPublishRedundantUIUpdates() {
        let controller = VPNController()
        controller.updateStatus(.connected)
        var count = 0
        let observer = controller.objectWillChange.sink { count += 1 }
        for _ in 0..<100 { controller.updateStatus(.connected) }
        XCTAssertEqual(count, 0)
        withExtendedLifetime(observer) {}
    }

    @MainActor
    func testFailedStartCannotLeaveToggleOnWhileChanging() {
        let controller = VPNController()
        controller.changing = true
        controller.updateStatus(.connecting)
        XCTAssertFalse(controller.enabled)
        controller.updateStatus(.disconnected)
        XCTAssertFalse(controller.enabled)
        XCTAssertFalse(controller.active)
        XCTAssertEqual(controller.status, "Disconnected")
        controller.changing = false
        XCTAssertFalse(controller.enabled)
    }

    @MainActor
    func testExternalDisconnectAndProfileRemovalClearToggle() {
        let controller = VPNController()
        for terminalState in [NEVPNStatus.disconnected, .invalid, .disconnecting, .reasserting] {
            controller.updateStatus(.connected)
            XCTAssertTrue(controller.enabled)
            controller.updateStatus(terminalState)
            XCTAssertFalse(controller.enabled)
            XCTAssertFalse(controller.active)
        }
    }

    private var deviceOnly: VPNConfiguration {
        VPNConfiguration(endpoint: "127.0.0.1:1234", token: "test", routes: [
            VPNRoute(selector: "mesh:device", name: "Lab", address: "10.99.1.2", host: "device-test.mesh.wendy.dev")
        ])
    }
    func testDeviceOnlyKeepsSplitRoutesAndDNS() {
        let settings = deviceOnly.networkSettings()
        XCTAssertEqual(settings.ipv4Settings?.includedRoutes?.map(\.destinationAddress), ["10.99.1.2", "100.64.0.53"])
        XCTAssertNil(settings.ipv6Settings)
        XCTAssertEqual(settings.dnsSettings?.matchDomains, ["mesh.wendy.dev", "cloud.wendy.dev"])
        XCTAssertEqual(settings.dnsSettings?.allowFailover, true)
    }
    func testInternetCapturesBothFamiliesAndUsesGatewayLease() throws {
        var configuration = deviceOnly
        configuration.internet = VPNInternetConfiguration(address4: "10.88.1.1", address6: "fd2b:677a:9d11::101", mtu: 1280)
        let roundTrip = try PropertyListDecoder().decode(VPNConfiguration.self, from: PropertyListEncoder().encode(configuration))
        let settings = roundTrip.networkSettings()
        XCTAssertEqual(settings.ipv4Settings?.addresses, ["10.88.1.1"])
        XCTAssertEqual(settings.ipv4Settings?.includedRoutes?.first?.destinationAddress, "0.0.0.0")
        XCTAssertEqual(settings.ipv6Settings?.addresses, ["fd2b:677a:9d11::101"])
        XCTAssertEqual(settings.ipv6Settings?.includedRoutes?.first?.destinationNetworkPrefixLength, 0)
        XCTAssertEqual(settings.dnsSettings?.matchDomains, [""])
        XCTAssertEqual(settings.dnsSettings?.allowFailover, false)
        XCTAssertEqual(settings.mtu, 1280)
    }
    func testIPv4OnlyDonorStillCapturesIPv6() {
        var configuration = deviceOnly
        configuration.internet = VPNInternetConfiguration(address4: "10.88.1.1", address6: nil, mtu: 1280)
        let settings = configuration.networkSettings()
        XCTAssertEqual(settings.ipv6Settings?.addresses, ["fd2b:677a:9d11::ffff"])
        XCTAssertEqual(settings.ipv6Settings?.includedRoutes?.first?.destinationNetworkPrefixLength, 0)
    }
}
