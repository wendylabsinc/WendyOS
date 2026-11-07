import XCTest

@MainActor
final class DevicesLensUITests: XCTestCase {
    private let app = XCUIApplication()

    private func launch(captureLauncher: Bool = false) {
        continueAfterFailure = false
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launchArguments = ["-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) {
            app.buttons["lens.continue"].tap()
        }
        let devices = app.buttons["lens.open.Devices"]
        XCTAssertTrue(devices.waitForExistence(timeout: 12))
        if captureLauncher { capture("General Home") }
        devices.tap()
    }

    func testFavoriteWidgetPickerStaysOpenWhileSearchingAndGoingBack() {
        continueAfterFailure = false
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) { app.buttons["lens.continue"].tap() }
        XCTAssertTrue(app.buttons["lens.open.Devices"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.otherElements["home.widgets.ready"].waitForExistence(timeout: 20))
        let add = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "Add widgets")).firstMatch
        if add.exists { add.tap() }
        else {
            let widget = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "home.widget.options.")).firstMatch
            XCTAssertTrue(widget.waitForExistence(timeout: 5))
            widget.press(forDuration: 0.7)
            app.buttons["Add widget"].tap()
        }
        for _ in 0..<3 {
            let favorite = app.buttons["Add Favorite devices"]
            XCTAssertTrue(favorite.waitForExistence(timeout: 5))
            favorite.tap()
            let search = app.textFields["home.favorites.search"]
            XCTAssertTrue(search.waitForExistence(timeout: 5))
            search.tap()
            search.typeText("NoSuchDevice123")
            XCTAssertTrue(app.staticTexts["No matching devices"].exists || app.staticTexts["No devices available"].exists)
            app.buttons["Clear search"].tap()
            XCTAssertTrue(search.exists)
            app.buttons["Back"].tap()
        }
        XCTAssertTrue(app.buttons["Close widget picker"].exists)
        app.buttons["Close widget picker"].tap()
        XCTAssertTrue(app.buttons["lens.open.Devices"].waitForExistence(timeout: 5))
    }

    func testWidgetTitleMenuEditsAndRemovesOnlyItsWidget() {
        func tapDone() {
            let button = app.buttons.matching(identifier: "Done").allElementsBoundByIndex.last(where: \.isHittable)
            XCTAssertNotNil(button)
            button?.tap()
        }
        continueAfterFailure = false
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) { app.buttons["lens.continue"].tap() }
        XCTAssertTrue(app.otherElements["home.widgets.ready"].waitForExistence(timeout: 20))
        let add = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "Add widgets")).firstMatch
        if add.exists { add.tap() }
        else {
            let header = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "home.widget.options.")).firstMatch
            XCTAssertTrue(header.waitForExistence(timeout: 5))
            header.press(forDuration: 0.7)
            XCTAssertTrue(app.buttons["Add widget"].waitForExistence(timeout: 5))
            app.buttons["Add widget"].tap()
        }
        let quick = app.buttons["Add Quick actions"]
        for _ in 0..<4 { if quick.isHittable { break }; app.swipeUp() }
        XCTAssertTrue(quick.isHittable)
        quick.tap()
        let title = app.textFields["Widget title"]
        XCTAssertTrue(title.waitForExistence(timeout: 5))
        title.tap(); title.typeText("Widget options QA")
        tapDone()
        // Adding returns to Home's layout editing mode.
        if app.buttons.matching(identifier: "Done").firstMatch.waitForExistence(timeout: 3) { tapDone() }
        let header = app.buttons["Widget options QA widget options"]
        for _ in 0..<6 { if header.isHittable { break }; app.swipeUp() }
        XCTAssertTrue(header.isHittable)
        header.tap()
        XCTAssertTrue(app.buttons["Edit widget"].waitForExistence(timeout: 5))
        app.buttons["Edit widget"].tap()
        XCTAssertTrue(title.waitForExistence(timeout: 5))
        XCTAssertEqual(title.value as? String, "Widget options QA")
        tapDone()
        XCTAssertTrue(header.waitForExistence(timeout: 5))
        header.tap()
        XCTAssertTrue(app.buttons["Remove widget"].waitForExistence(timeout: 5))
        app.buttons["Remove widget"].tap()
        XCTAssertFalse(header.waitForExistence(timeout: 2))
    }

    func testDeviceBottomRowsClearFloatingBars() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_BOTTOM_INSETS_FIXTURE"] == "1", "Requires local device fixture")
        continueAfterFailure = false
        app.launchEnvironment["WENDY_MOBILE_TEST_HOST"] = "0"
        app.launchArguments = ["-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["lens.continue"].waitForExistence(timeout: 2) { app.buttons["lens.continue"].tap() }
        if !app.tabBars.buttons["Devices"].waitForExistence(timeout: 3) {
            let open = app.buttons["lens.open.Devices"]
            XCTAssertTrue(open.waitForExistence(timeout: 15))
            open.tap()
        }
        XCTAssertTrue(app.tabBars.buttons["Devices"].waitForExistence(timeout: 15))
        app.tabBars.buttons["Devices"].tap()
        _ = selectInventoryRow("Devices UI QA")
        let settings = app.segmentedControls["device.sections"].buttons["Settings"]
        XCTAssertTrue(settings.waitForExistence(timeout: 30))
        settings.tap()
        let logs = app.buttons["device.settings.Logs"]
        let chat = app.buttons["lens.chat"]
        for _ in 0..<6 {
            if logs.exists && logs.isHittable && logs.frame.maxY <= chat.frame.minY { break }
            app.swipeUp()
        }
        XCTAssertTrue(logs.isHittable)
        XCTAssertLessThanOrEqual(logs.frame.maxY, chat.frame.minY, "Last Settings row must clear both floating bars")
        capture("Settings last row above bars")
        logs.tap()
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 10))
        app.navigationBars.buttons.firstMatch.tap()
        app.segmentedControls["device.sections"].buttons["Apps"].tap()
        let deploy = app.buttons.containing(.staticText, identifier: "Deploy an app").firstMatch
        for _ in 0..<6 {
            if deploy.exists && deploy.isHittable && deploy.frame.maxY <= chat.frame.minY { break }
            app.swipeUp()
        }
        XCTAssertTrue(deploy.isHittable)
        XCTAssertLessThanOrEqual(deploy.frame.maxY, chat.frame.minY, "Apps footer must clear both floating bars")
        capture("Apps footer above bars")
        deploy.tap()
        XCTAssertTrue(app.navigationBars["Deploy an app"].waitForExistence(timeout: 5))
        app.buttons["Done"].tap()
    }

    func testCloudModelSetupWithoutDeviceModel() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_CLOUD_CHAT_FIXTURE"] == "1", "Requires local device fixture; no cloud API calls")
        launch()
        _ = openDevice("Devices UI QA", source: "local")
        XCTAssertTrue(app.segmentedControls["device.sections"].buttons["Cameras"].waitForExistence(timeout: 30))
        app.buttons["lens.chat"].tap()
        XCTAssertTrue(app.buttons["Change device"].waitForExistence(timeout: 5))
        let inference = app.segmentedControls["chat.inference"]
        XCTAssertTrue(inference.waitForExistence(timeout: 10))
        let cloud = inference.buttons["Cloud"]
        let ready = NSPredicate(format: "enabled == true")
        expectation(for: ready, evaluatedWith: cloud)
        waitForExpectations(timeout: 30)
        cloud.tap()
        XCTAssertTrue(app.navigationBars["Cloud models"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.secureTextFields.firstMatch.exists)
        XCTAssertFalse(app.buttons["Save key & find models"].isEnabled)
        capture("Ask Wendy cloud model setup")
        app.buttons["Done"].tap()
        XCTAssertTrue(app.buttons["Connect cloud provider"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Send message"].isEnabled)
        capture("Ask Wendy cloud empty state")
        inference.buttons["On device"].tap()
        XCTAssertTrue(app.buttons["Browse device models"].waitForExistence(timeout: 30))
    }

    func testCameraFullscreenAndSupportedControls() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_CAMERAS_FIXTURE"] == "1", "Requires local Cameras fixture")
        launch()
        _ = openDevice("Devices UI QA", source: "local")
        XCTAssertTrue(app.segmentedControls["device.sections"].buttons["Cameras"].waitForExistence(timeout: 30))
        app.segmentedControls["device.sections"].buttons["Cameras"].tap()
        XCTAssertTrue(app.staticTexts["640 × 480"].firstMatch.waitForExistence(timeout: 30))
        capture("Camera tiles")
        app.buttons["Full screen QA Camera"].tap()
        XCTAssertTrue(app.buttons["Close full screen"].waitForExistence(timeout: 10))
        app.buttons["Pause QA Camera"].firstMatch.tap()
        XCTAssertTrue(app.staticTexts["Paused"].firstMatch.waitForExistence(timeout: 10))
        capture("Camera full screen paused")
        app.buttons["Play QA Camera"].firstMatch.tap()
        XCTAssertTrue(app.staticTexts["640 × 480"].firstMatch.waitForExistence(timeout: 15))
        app.buttons["Close full screen"].tap()
        app.buttons["Settings for QA Camera"].tap()
        let gain = app.textFields["Gain"]
        XCTAssertTrue(gain.waitForExistence(timeout: 10))
        gain.tap()
        gain.typeText(XCUIKeyboardKey.delete.rawValue + "8")
        app.buttons["Apply"].tap()
        XCTAssertTrue(app.staticTexts["Current 8"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["Read only"].exists)
        capture("Camera supported settings")
        app.buttons["Done"].tap()
        XCTAssertTrue(app.buttons["Full screen QA Camera"].waitForExistence(timeout: 10))
    }

    // Run with UITestSupport/appfixture and TEST_RUNNER_WENDY_UI_ACTIVITY_FIXTURE=1.
    func testActivityClipSelectionArchiveAndRestore() throws {
        guard ProcessInfo.processInfo.environment["WENDY_UI_ACTIVITY_FIXTURE"] == "1" else {
            throw XCTSkip("Requires the local Activity fixture.")
        }
        launch()
        app.tabBars.buttons["Activity"].tap()
        let preview = app.buttons["Play QA doorway"].firstMatch
        XCTAssertTrue(preview.waitForExistence(timeout: 60))
        preview.tap()
        XCTAssertTrue(app.buttons["data.play"].waitForExistence(timeout: 8))
        XCTAssertTrue(app.buttons["data.play"].isEnabled)
        XCTAssertTrue(app.buttons["data.episode.test-person"].isSelected)
        app.buttons["Close recordings"].tap()
        preview.swipeLeft()
        let archive = app.buttons["Archive"].firstMatch
        if archive.waitForExistence(timeout: 2) { archive.tap() }
        let archived = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Archived")).firstMatch
        archived.tap()
        let restore = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@ AND identifier ENDSWITH %@", "activity.restore.", ":clip.test-person")).firstMatch
        XCTAssertTrue(restore.waitForExistence(timeout: 5))
        restore.tap()
        app.buttons["Events"].tap()
        app.buttons["Alerts"].tap()
        XCTAssertTrue(preview.waitForExistence(timeout: 8))
        capture("Device Activity")
    }

    func testHomeOpensBothInventoriesAndReturnsAfterBackgrounding() {
        launch()
        for source in ["local", "cloud"] {
            let tile = app.buttons["devices.home.\(source)"]
            XCTAssertTrue(tile.waitForExistence(timeout: 12))
            tile.tap()
            let inventory = app.descendants(matching: .any)["devices.inventory"].firstMatch
            XCTAssertTrue(inventory.waitForExistence(timeout: 10))
            if source == "local" {
                XCUIDevice.shared.press(.home)
                app.activate()
                XCTAssertTrue(inventory.waitForExistence(timeout: 10))
                app.tabBars.buttons["Activity"].tap()
                app.tabBars.buttons["Home"].tap()
                XCTAssertTrue(inventory.waitForExistence(timeout: 10),
                              "Returning to Home should restore the local list.")
            }
            app.navigationBars.buttons.element(boundBy: 0).tap()
            XCTAssertTrue(tile.waitForExistence(timeout: 10))
        }
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = "Devices Home"
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    // Integration check. Supply TEST_RUNNER_WENDY_UI_DEVICE with an authorized
    // inventory row's display name; the simulator uses its own app credentials.
    func testLiveDeviceLogsAndReturnToInventory() throws {
        let environment = ProcessInfo.processInfo.environment
        guard let device = environment["WENDY_UI_DEVICE"], !device.isEmpty else {
            throw XCTSkip("Set TEST_RUNNER_WENDY_UI_DEVICE to run against a live device.")
        }
        let source = environment["WENDY_UI_SOURCE"] ?? "local"
        XCTAssertTrue(["local", "cloud"].contains(source))
        launch()
        let row = openDevice(device, source: source)
        let logs = app.buttons["devices.detail.logs"]
        guard logs.waitForExistence(timeout: 25) else {
            let details = app.buttons["Connection details"]
            if details.exists { details.tap() }
            XCTFail("Could not open the connected device overview: \(app.staticTexts.allElementsBoundByIndex.map(\.label))")
            return
        }
        logs.tap()
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.buttons.matching(identifier: "devices.logs.entry").firstMatch.waitForExistence(timeout: 25),
                      "The logs view opened, but no live device record arrived.")
        XCUIDevice.shared.press(.home)
        app.activate()
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 12))
        XCTAssertTrue(app.buttons.matching(identifier: "devices.logs.entry").firstMatch.waitForExistence(timeout: 25),
                      "The selected device’s log stream did not resume.")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(logs.waitForExistence(timeout: 12))
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(row.waitForExistence(timeout: 12))
    }

    func testAdvertisedSecureBoardOffersSignInOnSignedOutPhone() throws {
        guard let name = ProcessInfo.processInfo.environment["WENDY_UI_SECURE_DEVICE"], !name.isEmpty else {
            throw XCTSkip("Set TEST_RUNNER_WENDY_UI_SECURE_DEVICE on a signed-out test iPhone.")
        }
        launch()
        _ = openDevice(name, source: "local")
        XCTAssertTrue(app.buttons["Sign in to Wendy Cloud"].waitForExistence(timeout: 25))
        XCTAssertFalse(app.buttons["devices.detail.logs"].exists)
    }

    func testManageOrganizationPresentsSignInBrowser() throws {
        #if targetEnvironment(simulator) && DEBUG
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_SIGNED_OUT_LOGIN"] == "1",
                          "Opt in on a signed-out simulator with access to Wendy Cloud.")
        launch()
        app.buttons["lens.back"].tap()
        app.buttons["lens.accounts"].tap()
        app.buttons["Manage organization"].tap()
        let signIn = app.buttons["Sign in"]
        XCTAssertTrue(signIn.waitForExistence(timeout: 10))
        signIn.tap()
        XCTAssertTrue(app.buttons["Copy sign-in link for Mac"].waitForExistence(timeout: 30),
                      "The simulator sign-in browser should appear after Accounts closes.")
        #else
        throw XCTSkip("This regression check uses the debug simulator sign-in sheet.")
        #endif
    }

    func testDevicesTabsAndGeneralHome() {
        launch(captureLauncher: true)
        for tab in ["Home", "Devices", "Data", "Activity"] {
            XCTAssertTrue(app.tabBars.buttons[tab].exists)
        }
        XCTAssertFalse(app.tabBars.buttons["Sites"].exists)
        XCTAssertEqual(app.tabBars.count, 1, "The lens should expose one native tab bar.")
        app.tabBars.buttons["Devices"].tap()
        let picker = app.segmentedControls.firstMatch
        XCTAssertTrue(picker.waitForExistence(timeout: 5))
        picker.buttons["Mesh"].tap()
        picker.buttons["List"].tap()
        app.tabBars.buttons["Data"].tap()
        XCTAssertTrue(app.navigationBars["Data"].waitForExistence(timeout: 5))
        app.buttons["lens.chat"].tap()
        XCTAssertTrue(app.buttons["Change device"].waitForExistence(timeout: 5))
        app.buttons["Close"].tap()
        app.buttons["lens.back"].tap()
        XCTAssertTrue(app.buttons["lens.open.Devices"].waitForExistence(timeout: 8))
        XCTAssertTrue(app.buttons["lens.open.Sites"].exists)
    }

    func testDeviceAppLifecycleLogsAndWebPage() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["WENDY_UI_APPS_FIXTURE"] == "1",
                          "Start the loopback app fixture and set TEST_RUNNER_WENDY_UI_APPS_FIXTURE=1.")
        launch()
        _ = openDevice("Devices UI QA", source: "local")
        let row = app.buttons["device.apps.row.people-counter"]
        XCTAssertTrue(row.waitForExistence(timeout: 30))
        capture("Device Apps")
        row.tap()
        let toggle = app.buttons["app.toggle"]
        let open = app.buttons["app.open"]
        XCTAssertTrue(toggle.waitForExistence(timeout: 12))
        if toggle.label == "Stop" { toggle.tap() }
        waitForLabel("Start", button: toggle)
        XCTAssertFalse(open.isEnabled)
        toggle.tap()
        waitForLabel("Stop", button: toggle)
        XCTAssertTrue(open.isEnabled)
        capture("Running app")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(row.waitForExistence(timeout: 10))
        XCTAssertTrue(row.label.contains("Has a web page"))
        capture("Running device Apps")
        row.tap()
        app.buttons["app.restart"].tap()
        waitForLabel("Stop", button: toggle)
        app.buttons["app.logs"].tap()
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 15))
        let message = app.buttons.matching(NSPredicate(format: "identifier == %@ AND label CONTAINS %@",
                                                      "devices.logs.entry", "people-counter log record")).firstMatch
        XCTAssertTrue(message.waitForExistence(timeout: 20), "Expected the app-filtered stream.")
        capture("App logs")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(open.waitForExistence(timeout: 10))
        open.tap()
        XCTAssertTrue(app.webViews.firstMatch.staticTexts["Fixture dashboard online"].waitForExistence(timeout: 20),
                      "The selected app’s actual dashboard should load.")
        capture("App web page")
        app.buttons["app.browser.done"].tap()
        XCTAssertTrue(toggle.waitForExistence(timeout: 10))
        toggle.tap()
        waitForLabel("Start", button: toggle)
        XCTAssertFalse(open.isEnabled)
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(row.waitForExistence(timeout: 10))
        XCTAssertTrue(row.label.contains("STOPPED"))
    }

    func testAppAndLogsRecoverAfterDeviceDisconnects() async throws {
        guard let address = ProcessInfo.processInfo.environment["WENDY_UI_RECOVERY_URL"],
              let control = URL(string: address), control.host == "127.0.0.1" else {
            throw XCTSkip("Start the loopback app fixture with -recovery and set TEST_RUNNER_WENDY_UI_RECOVERY_URL.")
        }
        func online(_ active: Bool) async throws -> Int {
            var request = URLRequest(url: control.appendingPathComponent(active ? "online" : "offline"))
            request.httpMethod = "POST"
            let (data, response) = try await URLSession.shared.data(for: request)
            guard (response as? HTTPURLResponse)?.statusCode == 200,
                  let boot = Int(String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)) else {
                throw NSError(domain: "RecoveryFixture", code: 1)
            }
            return boot
        }
        _ = try await online(true)
        addTeardownBlock { @MainActor in _ = try await online(true) }
        launch()
        _ = openDevice("Devices UI QA", source: "local")
        let row = app.buttons["device.apps.row.people-counter"]
        XCTAssertTrue(row.waitForExistence(timeout: 30))
        row.tap()
        let toggle = app.buttons["app.toggle"]
        XCTAssertTrue(toggle.waitForExistence(timeout: 10))
        let initialAction = toggle.label
        _ = try await online(false)
        let disappeared = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: toggle)
        let offline = await XCTWaiter.fulfillment(of: [disappeared], timeout: 60)
        XCTAssertEqual(offline, .completed, "Stale controls remained visible on the disconnected device.")
        XCTAssertFalse(app.buttons["app.open"].exists)
        capture("App disconnected")
        let nextBoot = try await online(true)
        waitForLabel(initialAction, button: toggle, timeout: 75)
        app.buttons["app.logs"].tap()
        func record(_ boot: Int) -> XCUIElement {
            app.buttons.matching(NSPredicate(format: "identifier == %@ AND label CONTAINS %@",
                "devices.logs.entry", "people-counter log record (boot \(boot))")).firstMatch
        }
        XCTAssertTrue(record(nextBoot).waitForExistence(timeout: 15))
        _ = try await online(false)
        let lost = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: record(nextBoot))
        let cleared = await XCTWaiter.fulfillment(of: [lost], timeout: 60)
        XCTAssertEqual(cleared, .completed, "Records from the previous connection remained visible.")
        let lastBoot = try await online(true)
        XCTAssertTrue(record(lastBoot).waitForExistence(timeout: 75), "The same app log page should resume without reselection.")
        capture("App logs reconnected")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        waitForLabel(initialAction, button: toggle, timeout: 12)
    }

    func testInstalledSimpleAPILifecycleOverLAN() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard let device = environment["WENDY_UI_TEMPLATE_DEVICE"], !device.isEmpty,
              let address = environment["WENDY_UI_TEMPLATE_URL"], let endpoint = URL(string: address) else {
            throw XCTSkip("Set TEST_RUNNER_WENDY_UI_TEMPLATE_DEVICE and TEST_RUNNER_WENDY_UI_TEMPLATE_URL for the idle LAN test-node-simple-api template.")
        }
        launch()
        _ = openDevice(device, source: "local")
        XCTAssertTrue(app.navigationBars[device].waitForExistence(timeout: 25))
        let name = "test-node-simple-api"
        let row = app.buttons["device.apps.row." + name]
        XCTAssertTrue(row.waitForExistence(timeout: 30))
        XCTAssertTrue(row.label.contains("STOPPED"), "The test requires an explicitly stopped test app.")
        row.tap()
        let toggle = app.buttons["app.toggle"]
        waitForLabel("Start", button: toggle)
        let open = app.buttons["app.open"]
        XCTAssertFalse(open.isEnabled)
        toggle.tap()
        waitForLabel("Stop", button: toggle)
        addTeardownBlock { @MainActor in
            if self.app.buttons["app.browser.done"].exists { self.app.buttons["app.browser.done"].tap() }
            if self.app.navigationBars["Logs"].exists { self.app.navigationBars.buttons.element(boundBy: 0).tap() }
            if self.app.navigationBars[name].exists && toggle.exists && toggle.label == "Stop" {
                toggle.tap()
                self.waitForLabel("Start", button: toggle, timeout: 45)
            }
        }
        try await verifyAppLog(containing: "Received request: GET /", captureName: "LAN template logs") {
            // Old records may be unavailable. Generate a real request while this view subscribes.
            var request = URLRequest(url: endpoint, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 5)
            request.setValue("no-cache", forHTTPHeaderField: "Cache-Control")
            let (data, response) = try await URLSession.shared.data(for: request)
            XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 200)
            XCTAssertEqual(try JSONDecoder().decode([String: String].self, from: data)["message"], "hello-world")
        }
        verifyWebContent(containing: "hello-world", captureName: "LAN template web page")
        toggle.tap()
        waitForLabel("Start", button: toggle, timeout: 45)
        XCTAssertFalse(open.isEnabled)
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(row.waitForExistence(timeout: 8))
        XCTAssertTrue(row.label.contains("STOPPED"))
        capture("LAN template stopped")
    }

    func testRunningHelloPythonDashboardOverCloud() async throws {
        guard let device = ProcessInfo.processInfo.environment["WENDY_UI_HELLO_DEVICE"], !device.isEmpty else {
            throw XCTSkip("Set TEST_RUNNER_WENDY_UI_HELLO_DEVICE to the Cloud board already serving the HelloPython example.")
        }
        launch()
        _ = openDevice(device, source: "cloud")
        let name = "sh.wendy.examples.hellopython"
        XCTAssertTrue(app.buttons["devices.detail.logs"].waitForExistence(timeout: 30))
        openInstalledApp(name, badge: "Has a web page")
        verifyWebContent(containing: "Hello", captureName: "Cloud Hello Python web page")
        try await verifyAppLog(containing: "GET /", captureName: "Cloud Hello Python logs")
    }

    func testSwitchingDevicesIsolatesControlsLogsAndWebTunnel() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let local = env["WENDY_UI_SWITCH_LAN"], let cloud = env["WENDY_UI_SWITCH_CLOUD"] else {
            throw XCTSkip("Set TEST_RUNNER_WENDY_UI_SWITCH_LAN and TEST_RUNNER_WENDY_UI_SWITCH_CLOUD for the paired simulator check.")
        }
        let template = "test-node-simple-api"
        launch()
        _ = openDevice(local, source: "local")
        openInstalledApp(template, badge: "Has a web page")
        waitForLabel("Stop", button: app.buttons["app.toggle"])
        let lanPage = try XCTUnwrap(showDashboard(containing: "hello-world", captureName: "Switch - LAN dashboard"))
        app.buttons["app.browser.done"].tap()
        app.buttons["app.logs"].tap()
        try await verifyLiveTemplateLog(at: lanPage)

        app.tabBars.buttons["Devices"].tap()
        _ = selectInventoryRow(cloud)
        XCTAssertTrue(app.navigationBars[cloud].waitForExistence(timeout: 30))
        openInstalledApp(template, badge: "STOPPED")
        waitForLabel("Start", button: app.buttons["app.toggle"])
        let remoteOpen = app.buttons["app.open"]
        XCTAssertFalse(remoteOpen.exists && remoteOpen.isEnabled)
        capture("Switch - same app on Thor")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        openInstalledApp("sh.wendy.examples.hellopython", badge: "Has a web page")
        let tunnel = try XCTUnwrap(showDashboard(containing: "Hello", captureName: "Switch - Cloud dashboard"))
        XCTAssertEqual(tunnel.host, "127.0.0.1")
        app.buttons["app.browser.done"].tap()
        app.buttons["app.logs"].tap()
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 10))
        let (body, response) = try await fetch(tunnel)
        XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 200)
        XCTAssertTrue(String(decoding: body, as: UTF8.self).contains("Hello"))
        XCTAssertTrue(logRecord(containing: "GET /").waitForExistence(timeout: 25))
        _ = try await fetch(lanPage)
        XCTAssertFalse(logRecord(containing: "Received request: GET /").waitForExistence(timeout: 3))

        app.tabBars.buttons["Home"].tap()
        try await verifyLiveTemplateLog(at: lanPage)
        do {
            _ = try await fetch(tunnel)
            XCTFail("The previous Cloud tunnel still answered after switching back to the LAN device.")
        } catch let error as URLError {
            XCTAssertTrue([.cannotConnectToHost, .networkConnectionLost].contains(error.code), error.localizedDescription)
        }
        capture("Switch - restored LAN logs")
        app.navigationBars.buttons.element(boundBy: 0).tap()
        waitForLabel("Stop", button: app.buttons["app.toggle"])
    }

    private func openInstalledApp(_ name: String, badge: String) {
        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "device.apps.row."))
        XCTAssertTrue(rows.firstMatch.waitForExistence(timeout: 30))
        let row = app.buttons["device.apps.row." + name]
        let scroll = app.scrollViews.firstMatch
        if !row.exists || !row.isHittable {
            for _ in 0..<10 {
                if app.buttons["devices.detail.logs"].isHittable { break }
                scroll.swipeDown()
            }
        }
        for _ in 0..<18 {
            let top = app.navigationBars.firstMatch.frame.maxY
            let bottom = app.tabBars.buttons["Home"].frame.minY
            if row.exists && row.isHittable && row.frame.midY > top && row.frame.midY < bottom { break }
            if row.exists && !row.frame.isEmpty && row.frame.midY < top { scroll.swipeDown() } else { scroll.swipeUp() }
        }
        XCTAssertTrue(row.isHittable && row.label.contains(badge), "Expected " + name + " with badge " + badge)
        row.tap()
        XCTAssertTrue(app.navigationBars[name].waitForExistence(timeout: 10))
    }

    private func fetch(_ url: URL) async throws -> (Data, URLResponse) {
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 30)
        request.setValue("no-cache", forHTTPHeaderField: "Cache-Control")
        request.setValue("close", forHTTPHeaderField: "Connection")
        return try await URLSession.shared.data(for: request)
    }

    private func verifyLiveTemplateLog(at url: URL) async throws {
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 15))
        let records = app.buttons.matching(NSPredicate(format: "identifier == %@ AND label CONTAINS[c] %@",
                                                       "devices.logs.entry", "Received request: GET /"))
        let previous = records.allElementsBoundByIndex.map(\.label)
        let fresh = records.matching(NSPredicate(format: "NOT (label IN %@)", previous)).firstMatch
        for _ in 0..<5 {
            let (data, _) = try await fetch(url)
            XCTAssertEqual(try JSONDecoder().decode([String: String].self, from: data)["message"], "hello-world")
            if fresh.waitForExistence(timeout: 2) { return }
        }
        XCTFail("No live template request reached the selected LAN app's logs.")
    }

    private func logRecord(containing message: String) -> XCUIElement {
        app.buttons.matching(NSPredicate(format: "identifier == %@ AND label CONTAINS[c] %@",
                                         "devices.logs.entry", message)).firstMatch
    }

    private func verifyAppLog(containing message: String, captureName: String,
                              generateRequest: (() async throws -> Void)? = nil) async throws {
        let logs = app.buttons["app.logs"]
        XCTAssertTrue(logs.waitForExistence(timeout: 10))
        logs.tap()
        let record = logRecord(containing: message)
        XCTAssertTrue(app.navigationBars["Logs"].waitForExistence(timeout: 10))
        if let generateRequest {
            for _ in 0..<5 {
                try await generateRequest()
                if record.waitForExistence(timeout: 2) { break }
            }
        }
        XCTAssertTrue(record.waitForExistence(timeout: 25), "Expected an app-specific log record containing " + message)
        capture(captureName)
        app.navigationBars.buttons.element(boundBy: 0).tap()
    }

    private func verifyWebContent(containing marker: String, captureName: String) {
        _ = showDashboard(containing: marker, captureName: captureName)
        app.buttons["app.browser.done"].tap()
    }

    private func showDashboard(containing marker: String, captureName: String) -> URL? {
        let open = app.buttons["app.open"]
        XCTAssertTrue(open.waitForExistence(timeout: 10) && open.isEnabled)
        open.tap()
        let content = app.webViews.firstMatch.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS[c] %@ OR value CONTAINS[c] %@", marker, marker)).firstMatch
        XCTAssertTrue(content.waitForExistence(timeout: 30), "Expected content from the app itself: " + marker)
        capture(captureName)
        let address = app.staticTexts["app.browser.address"].value as? String
        return address.flatMap(URL.init(string:))
    }

    private func capture(_ title: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = title
        shot.lifetime = .keepAlways
        add(shot)
    }

    private func waitForLabel(_ title: String, button: XCUIElement, timeout: TimeInterval = 12) {
        let matches = NSPredicate(format: "label == %@ AND enabled == true", title)
        XCTAssertEqual(XCTWaiter.wait(for: [XCTNSPredicateExpectation(predicate: matches, object: button)], timeout: timeout), .completed)
    }


    private func openDevice(_ name: String, source: String) -> XCUIElement {
        let tile = app.buttons["devices.home.\(source)"]
        XCTAssertTrue(tile.waitForExistence(timeout: 15))
        tile.tap()
        return selectInventoryRow(name)
    }

    private func selectInventoryRow(_ name: String) -> XCUIElement {
        let search = app.searchFields.firstMatch
        if search.waitForExistence(timeout: 3) && search.isHittable {
            search.tap()
            search.typeText(name + "\n")
        }
        let row = app.buttons.matching(NSPredicate(format:
            "identifier BEGINSWITH %@ AND label CONTAINS[cd] %@", "devices.inventory.row.", name)).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 45), "Could not discover " + name + " in the current inventory.")
        row.tap()
        return row
    }

}
