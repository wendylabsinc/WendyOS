import XCTest

@MainActor final class DeviceAppearanceUITests: XCTestCase {
    func testPhotoPickerAndCrop() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_APPEARANCE_PHOTO_FIXTURE"] == "1", "Requires seeded QA photo and local UI fixture")
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launchArguments = ["-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        XCTAssertTrue(app.buttons["lens.open.Devices"].waitForExistence(timeout: 12))
        app.buttons["lens.open.Devices"].tap()
        app.buttons["devices.home.local"].tap()
        let row = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@ AND label CONTAINS %@", "devices.inventory.row.", "Devices UI QA")).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 45)); row.tap()
        openAppearance(app)
        XCTAssertTrue(app.buttons["appearance.photo"].waitForExistence(timeout: 5))
        app.buttons["appearance.photo"].tap()
        XCTAssertTrue(app.buttons["Cancel"].firstMatch.waitForExistence(timeout: 10))
        // The opt-in fixture must seed Wendy-QA-Device-Avatar.png as the newest photo.
        let photo = app.images.matching(NSPredicate(format: "label BEGINSWITH %@", "Photo,")).firstMatch
        XCTAssertTrue(photo.waitForExistence(timeout: 20))
        // Photos reports a valid frame but no hittable AX point on iOS 26.5.
        photo.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertTrue(app.navigationBars["Crop photo"].waitForExistence(timeout: 15))
        app.sliders["Photo zoom"].adjust(toNormalizedSliderPosition: 0.2)
        let crop = XCTAttachment(screenshot: app.screenshot()); crop.name = "Device photo crop"; crop.lifetime = .keepAlways; add(crop)
        app.buttons["Use photo"].tap()
        XCTAssertTrue(app.buttons["appearance.save"].waitForExistence(timeout: 5))
        let editor = XCTAttachment(screenshot: app.screenshot()); editor.name = "Device photo preview"; editor.lifetime = .keepAlways; add(editor)
        app.buttons["appearance.save"].tap()
        XCTAssertTrue(app.buttons["More device tools"].waitForExistence(timeout: 5))
        let result = XCTAttachment(screenshot: app.screenshot()); result.name = "Personalized device photo"; result.lifetime = .keepAlways; add(result)
        openAppearance(app)
        app.swipeUp()
        XCTAssertTrue(app.buttons["appearance.reset"].waitForExistence(timeout: 5))
        app.buttons["appearance.reset"].tap()
        app.buttons["appearance.save"].tap()
    }

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
        openAppearance(app)
        XCTAssertTrue(app.navigationBars["Device appearance"].waitForExistence(timeout: 5))
        app.buttons["Use 🚀"].tap()
        app.swipeUp()
        let blue = app.buttons["appearance.color.blue"]
        XCTAssertTrue(blue.waitForExistence(timeout: 3)); blue.tap()
        let screenshot = XCTAttachment(screenshot: app.screenshot()); screenshot.name = "Device appearance choices"; screenshot.lifetime = .keepAlways; add(screenshot)
        app.buttons["appearance.save"].tap()
        openAppearance(app)
        XCTAssertTrue(app.buttons["Use 🚀"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["Use 🚀"].isSelected)
        app.swipeUp()
        XCTAssertTrue(app.buttons["appearance.color.blue"].isSelected)
        app.buttons["appearance.reset"].tap()
        app.buttons["appearance.save"].tap()
        openAppearance(app)
        XCTAssertTrue(app.buttons["Use 🚀"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Use 🚀"].isSelected)
        app.buttons["Cancel"].tap()
    }
    private func openAppearance(_ app: XCUIApplication) {
        let tools = app.buttons["More device tools"]
        XCTAssertTrue(tools.waitForExistence(timeout: 30))
        tools.tap()
        let appearance = app.buttons["devices.appearance"]
        XCTAssertTrue(appearance.waitForExistence(timeout: 5))
        appearance.tap()
    }

}
