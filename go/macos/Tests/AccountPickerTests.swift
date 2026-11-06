import XCTest
@testable import WendyDesktop

@MainActor final class AccountPickerTests: XCTestCase {
    private func choice(_ id: String, name: String = "") -> AccountChoice {
        AccountChoice(id: id, email: "person@example.com", cloud: "https://cloud.example.com",
                      organizationID: id, organizationName: name, state: WD_ACCOUNT_SIGNED_IN, active: id == "one")
    }

    func testTenantScopedNames() async {
        let picker = AccountPickerModel()
        var requested: [String] = []
        await picker.load(readChoices: { [self.choice("one"), self.choice("two")] }, organizations: { id in
            requested.append(id)
            return [CloudOrganization(id: id, name: "Team " + id)]
        })
        XCTAssertEqual(Set(requested), ["one", "two"])
        XCTAssertEqual(picker.choices.map(\.organizationTitle), ["Team one", "Team two"])
        XCTAssertNil(picker.error)
    }

    func testFailedSessionDoesNotHideOtherNames() async {
        let picker = AccountPickerModel()
        await picker.load(readChoices: { [self.choice("one"), self.choice("two")] }, organizations: { id in
            if id == "one" { throw URLError(.notConnectedToInternet) }
            return [CloudOrganization(id: id, name: "Robotics")]
        })
        XCTAssertEqual(picker.choices.map(\.organizationTitle), ["Organization one", "Robotics"])
        XCTAssertNotNil(picker.error)
        XCTAssertTrue(picker.choices[0].active)
    }

    func testCachedNamesAndDistinctFallbacks() async {
        XCTAssertNotEqual(choice("one", name: "  ").organizationTitle, choice("two").organizationTitle)
        let picker = AccountPickerModel()
        await picker.load(readChoices: { [self.choice("one", name: "Robotics")] }, organizations: { _ in
            XCTFail("Cached names should remain available offline")
            return []
        })
        XCTAssertEqual(picker.active?.organizationTitle, "Robotics")
    }
}
