import XCTest

@MainActor final class SitesLensUITests: XCTestCase {
    func testCreateSitePersistsAndModelEntryUsesNativeSheets() {
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launchEnvironment["WENDY_SITES_UI_TEST"] = UUID().uuidString
        app.launchArguments = ["-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) { app.buttons["lens.continue"].tap() }
        XCTAssertTrue(app.buttons["lens.open.Sites"].waitForExistence(timeout: 15))
        app.buttons["lens.open.Sites"].tap()
        XCTAssertTrue(app.buttons["Create Site"].waitForExistence(timeout: 10))
        for title in ["Home", "Missions", "Activity"] { XCTAssertTrue(app.tabBars.buttons[title].exists) }
        app.buttons["Create Site"].tap()
        let name = app.textFields["sites.name"]
        XCTAssertTrue(name.waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Create"].isEnabled)
        name.tap(); name.typeText("Sites QA Office")
        let assets = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Devices")).firstMatch
        assets.tap()
        XCTAssertTrue(app.navigationBars["Devices"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.searchFields["Search by name"].exists)
        app.navigationBars.buttons.firstMatch.tap()
        app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Robots")).firstMatch.tap()
        XCTAssertTrue(app.navigationBars["Robots"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.searchFields["Search by name"].exists)
        app.navigationBars.buttons.firstMatch.tap()
        XCTAssertTrue(app.buttons["Create"].waitForExistence(timeout: 5))
        app.buttons["Create"].tap()
        let addModel = app.buttons["Add a 3D model"].firstMatch
        XCTAssertTrue(addModel.waitForExistence(timeout: 5))
        let edit = app.buttons["Edit devices and robots"]
        XCTAssertTrue(edit.waitForExistence(timeout: 5))
        edit.tap()
        XCTAssertTrue(app.navigationBars["Edit devices and robots"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["Save"].exists)
        app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Devices")).firstMatch.tap()
        XCTAssertTrue(app.searchFields["Search by name"].exists)
        XCTAssertTrue(app.buttons["Unselect All"].exists)
        app.navigationBars.buttons.firstMatch.tap()
        app.buttons["Save"].tap()
        capture(app, "Sites setup checklist")
        addModel.tap()
        XCTAssertTrue(app.buttons["Import a file"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Scan with iPhone"].isEnabled, "Simulator must not offer unsupported RoomPlan capture")
        capture(app, "Add site model")
        app.buttons["Cancel"].tap()
        app.buttons["Load from device"].tap()
        XCTAssertTrue(app.navigationBars["Load from device"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.searchFields["Search devices"].exists)
        app.buttons["Done"].tap()
        app.tabBars.buttons["Activity"].tap()
        XCTAssertTrue(app.staticTexts["Sites QA Office created"].waitForExistence(timeout: 5))
        app.terminate(); app.launch()
        XCTAssertTrue(app.buttons["lens.open.Sites"].waitForExistence(timeout: 15))
        app.buttons["lens.open.Sites"].tap()
        XCTAssertTrue(app.buttons["Add a 3D model"].firstMatch.waitForExistence(timeout: 10))
        XCTAssertFalse(app.tabBars.buttons["Sites"].exists)
        XCTAssertTrue(app.staticTexts["Sites QA Office"].waitForExistence(timeout: 5))
    }
    private func capture(_ app: XCUIApplication, _ title: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = title; attachment.lifetime = .keepAlways
        add(attachment)
    }
}
