import XCTest
@testable import WendySpatial

final class SpatialConnectionRecoveryTests: XCTestCase {
    func testSignedOutPrefaceFailureOffersSignIn() {
        XCTAssertTrue(SpatialConnectionRecovery.needsLogin(signedIn: false,
            error: "rpc error: code = Unavailable desc = connection error: error reading server preface: EOF"))
    }
    func testSignedInOrNetworkFailuresDoNotClaimMissingLogin() {
        XCTAssertFalse(SpatialConnectionRecovery.needsLogin(signedIn: true, error: "error reading server preface: EOF"))
        for message in ["no route to host", "connection refused", "context deadline exceeded", "device identity changed"] {
            XCTAssertFalse(SpatialConnectionRecovery.needsLogin(signedIn: false, error: message))
        }
    }
}
