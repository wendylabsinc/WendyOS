import XCTest
@testable import WendyMobile

final class MobileTests: XCTestCase {
    func testEmbeddedSignInProducesBrowserURL() async throws {
        let client = GoClient.shared
        try await client.initializeMobile()
        try await client.login(development: false)
        do {
            var loginURL: URL?
            for _ in 0..<100 {
                for event in try await client.poll() {
                    switch event.payload {
                    case .loginURL(let value): loginURL = URL(string: value)
                    case .loginError(let message): throw WendyError(message: message)
                    default: break
                    }
                }
                if loginURL != nil { break }
                try await Task.sleep(for: .milliseconds(50))
            }
            let url = try XCTUnwrap(loginURL, "Sign-in must emit a URL for the Safari sheet")
            XCTAssertEqual(url.scheme, "https")
            XCTAssertTrue(url.path.hasSuffix("/authorize"))
            try await client.cancelLogin()
        } catch {
            try? await client.cancelLogin()
            throw error
        }
    }

    @MainActor
    func testOrganizationFailureKeepsSavedAccountAndRetryRecovers() async {
        let model = AppModel()
        let saved = SavedAccount(account: CloudAccount(
            state: WD_ACCOUNT_SIGNED_IN, context: "test-account",
            cloud: "https://cloud.example", email: "test@example.com"
        ), active: true)
        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { account in
                XCTAssertEqual(account, saved.id)
                XCTAssertEqual(model.mobileCloudAccounts.map(\.id), [saved.id])
                throw WendyError(message: "Organization service unavailable")
            }
        )
        XCTAssertEqual(model.mobileCloudAccounts.map(\.id), [saved.id])
        XCTAssertEqual(model.mobileCloudAccount, saved.id)
        XCTAssertEqual(model.mobileCloudSelectionError, "Organization service unavailable")
        XCTAssertFalse(model.mobileCloudSelectionLoading)

        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { _ in [CloudOrganization(id: "org", name: "Test organization")] }
        )
        XCTAssertEqual(model.mobileCloudOrganization, "org")
        XCTAssertNil(model.mobileCloudSelectionError)
        XCTAssertFalse(model.mobileCloudSelectionLoading)
    }

    @MainActor
    func testAccountChangeClearsPreviousOrganizationBeforeFailedRead() async {
        let model = AppModel()
        model.mobileCloudAccount = "old-account"
        model.mobileCloudOrganization = "old-org"
        model.mobileCloudOrganizations = [CloudOrganization(id: "old-org", name: "Old organization")]
        let saved = SavedAccount(account: CloudAccount(
            state: WD_ACCOUNT_SIGNED_IN, context: "new-account",
            cloud: "https://cloud.example", email: "test@example.com"
        ), active: true)
        await model.refreshMobileCloudSelection(
            loadAccounts: { [saved] },
            loadOrganizations: { _ in throw WendyError(message: "Offline") }
        )
        XCTAssertEqual(model.mobileCloudAccount, saved.id)
        XCTAssertTrue(model.mobileCloudOrganizations.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
        XCTAssertEqual(model.mobileCloudSelectionError, "Offline")
    }

    @MainActor
    func testSignedOutRefreshClearsOrganizationWithoutCloudRequest() async {
        let model = AppModel()
        model.mobileCloudOrganization = "stale-org"
        model.mobileCloudOrganizations = [CloudOrganization(id: "stale-org", name: "Stale organization")]
        await model.refreshMobileCloudSelection(
            loadAccounts: { [] },
            loadOrganizations: { _ in XCTFail("Signed-out refresh should not call Cloud"); return [] }
        )
        XCTAssertTrue(model.mobileCloudAccounts.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganizations.isEmpty)
        XCTAssertTrue(model.mobileCloudOrganization.isEmpty)
        XCTAssertNil(model.mobileCloudSelectionError)
    }

    func testEmbeddedGoInitializesInAppSandbox() async throws {
        try await GoClient.shared.initializeMobile()
        let account = try await GoClient.shared.account()
        XCTAssertNotNil(account.title)
        // Polling crosses the C archive without a CLI process or device connection.
        let events = try await GoClient.shared.poll()
        XCTAssertTrue(events.isEmpty)
    }

    func testLocalVideoParserAcceptsSplitStartCodes() {
        var parser = AnnexBParser()
        XCTAssertTrue(parser.append(Data([0, 0])).isEmpty)
        let nals = parser.append(Data([1, 0x65, 0x11, 0, 0, 1, 0x41]))
        XCTAssertEqual(nals, [Data([0x65, 0x11])])
    }

    func testRecordedVideoUsesEpisodeTimestampSchedule() {
        let times = [-2.0, -1.8, -1.5, 0.2]
        XCTAssertNil(DataVisualSearch.latest(times, at: -3, time: { $0 }))
        XCTAssertEqual(DataVisualSearch.latest(times, at: -1.6, time: { $0 }), -1.8)
        XCTAssertEqual(DataVisualSearch.latest(times, at: 2, time: { $0 }), 0.2)
    }
}
