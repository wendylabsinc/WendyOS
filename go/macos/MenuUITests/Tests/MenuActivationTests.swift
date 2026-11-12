import XCTest

@MainActor
final class MenuActivationTests: XCTestCase {
    private let shortName = "regression-fixture"
    private let longName = "regression-fixture-with-a-long-name-that-constrains-the-action-column"
    // Points just inside every edge and corner remain outside the centered icon.
    private let points: [(String, CGVector)] = [
        ("left", CGVector(dx: 0.05, dy: 0.5)), ("right", CGVector(dx: 0.95, dy: 0.5)),
        ("top", CGVector(dx: 0.5, dy: 0.05)), ("bottom", CGVector(dx: 0.5, dy: 0.95)),
        ("top-left", CGVector(dx: 0.05, dy: 0.05)), ("top-right", CGVector(dx: 0.95, dy: 0.05)),
        ("bottom-left", CGVector(dx: 0.05, dy: 0.95)), ("bottom-right", CGVector(dx: 0.95, dy: 0.95)),
    ]

    override func setUpWithError() throws { continueAfterFailure = false }

    private func launch(_ arguments: [String]) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = arguments
        app.launch()
        addTeardownBlock { app.terminate() }
        return app
    }

    private func menu(_ app: XCUIApplication, label: String) -> XCUIElement {
        let element = app.descendants(matching: .any).matching(identifier: label).firstMatch
        XCTAssertTrue(element.waitForExistence(timeout: 10), app.debugDescription)
        return element
    }

    private func click(_ control: XCUIElement, point: CGVector) {
        // Use the intended 44-point hit target even when the accessible frame
        // shrinks in the negative control. Normalizing directly to the reported
        // frame would silently turn the edge test into another icon click.
        control.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5))
            .withOffset(CGVector(dx: (point.dx - 0.5) * 44, dy: (point.dy - 0.5) * 44)).click()
    }

    private func appEdges(long: Bool) {
        let name = long ? longName : shortName
        let app = launch(long ? ["--long-name", "--narrow"] : [])
        for (edge, point) in points {
            XCTContext.runActivity(named: "App row \(edge)") { _ in
                let control = menu(app, label: "Actions for \(name)")
                XCTAssertTrue(control.isHittable)
                click(control, point: point)
                let remove = app.menuItems["Remove App…"]
                XCTAssertTrue(remove.waitForExistence(timeout: 3), "Edge click did not open the production app menu")
                remove.click()
                let requested = app.staticTexts["requested-app-deletion"]
                XCTAssertTrue(requested.waitForExistence(timeout: 3))
                XCTAssertEqual(requested.label, "Deletion requested: \(name)")
                app.buttons["Dismiss deletion request"].click()
                XCTAssertFalse(requested.exists)
            }
        }
    }

    private func simulatorEdges(long: Bool) {
        let name = long ? longName : shortName
        let app = launch(["--simulators"] + (long ? ["--long-name", "--narrow"] : []))
        for (edge, point) in points {
            XCTContext.runActivity(named: "Simulator header \(edge)") { _ in
                let control = menu(app, label: "Simulator options")
                XCTAssertTrue(control.isHittable)
                click(control, point: point)
                let remove = app.menuItems["Delete simulator…"]
                XCTAssertTrue(remove.waitForExistence(timeout: 3), "Edge click did not open the production simulator menu")
                remove.click()
                let confirmation = app.dialogs.firstMatch
                XCTAssertTrue(confirmation.waitForExistence(timeout: 3))
                XCTAssertTrue(confirmation.staticTexts["Delete \(name), its disk, snapshots and console logs? This cannot be undone."].exists)
                confirmation.buttons["Cancel"].click()
                XCTAssertFalse(confirmation.exists)
            }
        }
    }

    func testAppMenuEdges() { appEdges(long: false) }
    func testAppMenuEdgesLongName() { appEdges(long: true) }
    func testSimulatorMenuEdges() { simulatorEdges(long: false) }
    func testSimulatorMenuEdgesLongName() { simulatorEdges(long: true) }

    func testBusyAppCannotBeRemoved() {
        let app = launch(["--busy"])
        click(menu(app, label: "Actions for \(shortName)"), point: CGVector(dx: 0.05, dy: 0.5))
        let remove = app.menuItems["Remove App…"]
        XCTAssertTrue(remove.waitForExistence(timeout: 3))
        XCTAssertFalse(remove.isEnabled)
        XCTAssertFalse(app.staticTexts["requested-app-deletion"].exists)
    }

    func testBusySimulatorMenuIsDisabled() {
        let app = launch(["--simulators", "--busy"])
        let control = menu(app, label: "Simulator options")
        XCTAssertFalse(control.isEnabled)
        click(control, point: CGVector(dx: 0.05, dy: 0.5))
        XCTAssertFalse(app.menuItems["Delete simulator…"].waitForExistence(timeout: 1))
    }
}
