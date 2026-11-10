import XCTest
@testable import WendyDesktop

final class DeviceExperienceTests: XCTestCase {
    func testTypedRecoveryDoesNotDependOnDiagnosticWording() {
        let expired = WendyError(message: "arbitrary wording", code: .credentialsExpired, remedy: .signIn)
        XCTAssertTrue(PresentedFailure(expired).offersDeviceSignIn)
        XCTAssertFalse(ConnectionRecovery.canRetry(expired))
        let unknown = WendyError(message: "sign in: certificate expired: connection refused", code: 999, remedy: 999)
        XCTAssertEqual(unknown.code, .unknown)
        XCTAssertEqual(unknown.remedy, .none)
        XCTAssertFalse(PresentedFailure(unknown).offersDeviceSignIn)
        XCTAssertFalse(ConnectionRecovery.canRetry(unknown))
        let network = WendyError(message: "permission denied", code: .networkUnavailable, remedy: .retry)
        XCTAssertTrue(ConnectionRecovery.canRetry(network))
        let peer = WendyError(message: "expired certificate", code: .secureConnectionRejected, remedy: .checkClock)
        XCTAssertFalse(PresentedFailure(peer).offersDeviceSignIn)
        XCTAssertFalse(peer.canForgetIdentity)
        XCTAssertTrue(PresentedFailure(WendyError(message: "", code: .localNetworkDenied, remedy: .openNetworkSettings)).offersNetworkSettings)
        XCTAssertFalse(PresentedFailure(WendyError(message: "Bearer secret-token")).details.contains("secret-token"))
    }

    func testGoStatusPreservesFailureMetadata() throws {
        var status = WDStatus()
        try "diagnostic".withCString { text in
            status.message = UnsafeMutablePointer(mutating: text)
            status.error_code = FailureCode.credentialsExpired.rawValue
            status.remedy = FailureRemedy.signIn.rawValue
            XCTAssertThrowsError(try GoClient.check(status)) { error in
                XCTAssertEqual((error as? WendyError)?.code, .credentialsExpired)
                XCTAssertEqual((error as? WendyError)?.remedy, .signIn)
                XCTAssertEqual((error as? WendyError)?.canForgetIdentity, false)
            }
        }
    }

    @MainActor
    func testConnectionAndLoginStateKeepTypedErrors() async {
        let failure = WendyError(message: "diagnostic", code: .credentialsExpired, remedy: .signIn)
        let model = AppModel { _, _, _ in throw failure }
        await model.connect(Device(id: "test", name: "Test", selector: "cloud://test", transport: .cloud, address: ""))
        XCTAssertEqual(model.detailError, failure)
        XCTAssertFalse(model.canForgetIdentity)
        let events = AsyncStream<Void>.makeStream()
        defer { events.continuation.finish() }
        model.signingIn = true
        model.receive(.init(session: 0, stream: 0, payload: .loginError(failure)), refreshes: events.continuation)
        XCTAssertEqual(model.alertFailure, failure)
        XCTAssertFalse(model.signingIn)
    }

    func testFailureABIValuesMatchC() {
        let codes: [(FailureCode, Int)] = [
            (.unknown, WD_ERROR_UNKNOWN), (.authenticationRequired, WD_ERROR_AUTHENTICATION_REQUIRED),
            (.credentialsExpired, WD_ERROR_CREDENTIALS_EXPIRED), (.accountMismatch, WD_ERROR_ACCOUNT_MISMATCH),
            (.accessDenied, WD_ERROR_ACCESS_DENIED), (.identityMismatch, WD_ERROR_IDENTITY_MISMATCH),
            (.secureConnectionRejected, WD_ERROR_SECURE_CONNECTION_REJECTED), (.networkUnavailable, WD_ERROR_NETWORK_UNAVAILABLE),
            (.localNetworkDenied, WD_ERROR_LOCAL_NETWORK_DENIED), (.agentUpdateRequired, WD_ERROR_AGENT_UPDATE_REQUIRED),
            (.storageFull, WD_ERROR_STORAGE_FULL), (.portInUse, WD_ERROR_PORT_IN_USE),
            (.serviceBusy, WD_ERROR_SERVICE_BUSY), (.cancelled, WD_ERROR_CANCELLED)
        ]
        let remedies: [(FailureRemedy, Int)] = [
            (.none, WD_REMEDY_NONE), (.retry, WD_REMEDY_RETRY), (.signIn, WD_REMEDY_SIGN_IN),
            (.chooseAccount, WD_REMEDY_CHOOSE_ACCOUNT), (.openNetworkSettings, WD_REMEDY_OPEN_NETWORK_SETTINGS),
            (.checkClock, WD_REMEDY_CHECK_CLOCK), (.updateAgent, WD_REMEDY_UPDATE_AGENT),
            (.reviewIdentity, WD_REMEDY_REVIEW_IDENTITY), (.freeStorage, WD_REMEDY_FREE_STORAGE),
            (.choosePort, WD_REMEDY_CHOOSE_PORT), (.wait, WD_REMEDY_WAIT)
        ]
        for (value, expected) in codes { XCTAssertEqual(Int(value.rawValue), expected) }
        for (value, expected) in remedies { XCTAssertEqual(Int(value.rawValue), expected) }
    }

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
        feed.fail(WendyError(message: "connection reset", code: .networkUnavailable, remedy: .retry), stream: 1)
        XCTAssertTrue(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        feed.pause(manually: true)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        feed.allowAutoplay()
        feed.start(stream: 2)
        feed.fail(WendyError(message: "permission denied", code: .accessDenied, remedy: .chooseAccount), stream: 2)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
    }
}
