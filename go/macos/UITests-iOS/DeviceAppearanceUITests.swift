import XCTest

@MainActor final class DeviceAppearanceUITests: XCTestCase {
    func testPersonalIconPersistsAndResets() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_APPEARANCE_FIXTURE"] == "1", "Requires local UI fixture")
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launchArguments = ["-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) { app.buttons["lens.continue"].tap() }
        XCTAssertTrue(app.buttons["lens.open.Devices"].waitForExistence(timeout: 12))
        app.buttons["lens.open.Devices"].tap()
        app.buttons["devices.home.local"].tap()
        let row = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@ AND label CONTAINS %@", "devices.inventory.row.", "Devices UI QA")).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 45)); row.tap()
        let avatar = app.buttons["devices.appearance.avatar"]
        XCTAssertTrue(avatar.waitForExistence(timeout: 30)); avatar.tap()
        XCTAssertTrue(app.navigationBars["Device appearance"].waitForExistence(timeout: 5))
        app.buttons["Use 🚀"].tap()
        app.swipeUp()
        let blue = app.buttons["appearance.color.blue"]
        XCTAssertTrue(blue.waitForExistence(timeout: 3)); blue.tap()
        let screenshot = XCTAttachment(screenshot: app.screenshot()); screenshot.name = "Device appearance choices"; screenshot.lifetime = .keepAlways; add(screenshot)
        app.buttons["appearance.save"].tap()
        XCTAssertTrue(avatar.waitForExistence(timeout: 5)); avatar.tap()
        XCTAssertTrue(app.buttons["Use 🚀"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["Use 🚀"].isSelected)
        app.swipeUp()
        XCTAssertTrue(app.buttons["appearance.color.blue"].isSelected)
        app.buttons["appearance.reset"].tap()
        app.buttons["appearance.save"].tap()
        XCTAssertTrue(avatar.waitForExistence(timeout: 5)); avatar.tap()
        XCTAssertTrue(app.buttons["Use 🚀"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Use 🚀"].isSelected)
        app.buttons["Cancel"].tap()
    }
}
