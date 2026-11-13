import XCTest
@testable import WendyDesktop

final class DeviceExperienceTests: XCTestCase {
    func testDeviceDisplayNameKeepsCustomNamesAndAddressesIntact() {
        XCTAssertEqual(DeviceDisplayName.title("wendyos-iq9-mesh"), "IQ9 Mesh")
        XCTAssertEqual(DeviceDisplayName.title("Kitchen Camera"), "Kitchen Camera")
        XCTAssertEqual(DeviceDisplayName.title("my-custom-name"), "my-custom-name")
        XCTAssertEqual(DeviceDisplayName.title("169.254.243.154"), "169.254.243.154")
    }

    func testRecoveryNeverTreatsTrustOrAuthorizationAsAConnectionRetry() {
        for error in ["certificate verification failed: connection closed", "identity mismatch", "permission denied: unavailable", "Unauthenticated", "Sign in again", "local network access denied", "Update this device"] {
            XCTAssertFalse(ConnectionRecovery.canRetry(error), error)
        }
        for error in ["Bluetooth connection ended; retry setup", "network is unreachable", "rpc error: unavailable", "connection reset by peer", "i/o timeout", "EOF"] {
            XCTAssertTrue(ConnectionRecovery.canRetry(error), error)
        }
        XCTAssertFalse(ConnectionRecovery.canRetry("Unknown operation"))
    }

    func testOrganizationDefaultUsesCurrentAccountAndDoesNotGuessAmongTenants() {
        let a = EnrollmentOrganization(account: "a", organizationID: "one", name: "One", email: "a@example.com")
        let b = EnrollmentOrganization(account: "b", organizationID: "two", name: "Two", email: "b@example.com")
        XCTAssertEqual(DeviceEnrollmentForm.defaultOrganization([a, b], currentAccount: "b"), "b")
        XCTAssertEqual(DeviceEnrollmentForm.defaultOrganization([a], currentAccount: "missing"), "a")
        XCTAssertEqual(DeviceEnrollmentForm.defaultOrganization([a, b], currentAccount: "missing"), "")
        XCTAssertEqual(DeviceEnrollmentForm.defaultOrganization([], currentAccount: "a"), "")
    }

    @MainActor
    func testOrdinaryCameraRecoversButRespectsPauseAndAuthorization() {
        let feed = CameraFeed(camera: Camera(id: 1, name: "Camera"))
        feed.start(stream: 1)
        feed.fail("connection reset", stream: 1)
        XCTAssertTrue(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        feed.pause(manually: true)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        feed.allowAutoplay()
        feed.start(stream: 2)
        feed.fail("permission denied", stream: 2)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
    }
}
