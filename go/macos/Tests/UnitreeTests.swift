import XCTest
import AppKit
@testable import WendyDesktop

final class UnitreeTests: XCTestCase {
    func testKeyboardAliasesAndOpposingKeys() {
        XCTAssertEqual(UnitreeMotion.keys([13]), UnitreeMotion.keys([126]))
        XCTAssertEqual(UnitreeMotion.keys([1]), UnitreeMotion.keys([125]))
        XCTAssertEqual(UnitreeMotion.keys([0]), UnitreeMotion.keys([123]))
        XCTAssertEqual(UnitreeMotion.keys([2]), UnitreeMotion.keys([124]))
        XCTAssertEqual(UnitreeMotion.keys([13, 126]).forward, 0.6)
        XCTAssertEqual(UnitreeMotion.keys([13, 1]).forward, 0)
        XCTAssertEqual(UnitreeMotion.keys([0, 2]).lateral, 0)
        XCTAssertEqual(UnitreeMotion.keys([12]).yaw, 0.5)
        XCTAssertEqual(UnitreeMotion.keys([14]).yaw, -0.5)
        XCTAssertEqual(UnitreeMotion.keys([12, 14]).yaw, 0)
        XCTAssertEqual(UnitreeMotion.keys([]), UnitreeMotion())
        XCTAssertEqual(UnitreeMotion.keys([13, 2, 12]), UnitreeMotion(forward: 0.6, lateral: -0.3, yaw: 0.5))
    }

    func testRememberedRobotSurvivesLaunchWithoutConnectionState() throws {
        let suite = "UnitreeTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let robot = UnitreeRobot(serial: "GO2-123", name: "Go2", address: "192.168.1.2", paired: true, connected: true)
        UnitreeSavedDevices.save([robot], defaults: defaults)
        let saved = try XCTUnwrap(UnitreeSavedDevices.load(defaults: defaults).first)
        XCTAssertEqual(saved.serial, robot.serial)
        XCTAssertEqual(saved.address, robot.address)
        XCTAssertFalse(saved.connected)
        XCTAssertFalse(saved.paired)
        let data = try XCTUnwrap(defaults.data(forKey: UnitreeSavedDevices.key))
        let objects = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [[String: String]])
        XCTAssertEqual(Set(objects[0].keys), ["serial", "name", "address"])
    }

    func testDiscoveryUpdatesAddressWithoutRemovingOfflineSavedRobots() {
        let saved = UnitreeRobot(serial: "GO2-123", name: "Go2", address: "192.168.1.2", paired: false, connected: false)
        let fresh = UnitreeRobot(serial: saved.serial, name: saved.name, address: "192.168.1.3", paired: false, connected: false)
        XCTAssertEqual(UnitreeSavedDevices.merge(saved: [saved], discovered: []), [saved])
        XCTAssertEqual(UnitreeSavedDevices.merge(saved: [saved], discovered: [fresh]), [fresh])
    }
}

@MainActor
final class UnitreeMotionDriverTests: XCTestCase {
    func testCombinedKeysCoalesceIntoOneVelocity() async {
        var sent: [UnitreeMotion] = []
        let driver = UnitreeMotionDriver { sent.append($0) }
        driver.enabled = true
        driver.update(.keys([13]), direction: "Keyboard")
        driver.update(.keys([13, 12]), direction: "Keyboard")
        await driver.step(now: Date(timeIntervalSince1970: 1))
        XCTAssertEqual(sent, [UnitreeMotion(forward: 0.6, lateral: 0, yaw: 0.5)])
        XCTAssertTrue(driver.enabled)
        driver.update(.keys([12]), direction: "Keyboard")
        await driver.step(now: Date(timeIntervalSince1970: 1.05))
        XCTAssertEqual(sent.count, 1)
        await driver.step(now: Date(timeIntervalSince1970: 1.11))
        XCTAssertEqual(sent.last, UnitreeMotion(yaw: 0.5))
    }

    func testFocusLossStopsWithoutDisablingOrResuming() async {
        var sent: [UnitreeMotion] = []
        let driver = UnitreeMotionDriver { sent.append($0) }
        driver.enabled = true
        driver.update(.keys([13, 14]), direction: "Keyboard")
        await driver.step(now: Date(timeIntervalSince1970: 1))
        driver.stop() // Focus loss, with keys still physically held.
        await driver.step(now: Date(timeIntervalSince1970: 1.01))
        XCTAssertEqual(sent.last, UnitreeMotion())
        XCTAssertTrue(driver.enabled)
        XCTAssertEqual(driver.desired, UnitreeMotion())
        await driver.step(now: Date(timeIntervalSince1970: 2))
        XCTAssertEqual(sent.count, 2) // Refocusing alone must not resume movement.
        driver.update(.keys([12]), direction: "Keyboard")
        await driver.step(now: Date(timeIntervalSince1970: 3))
        XCTAssertEqual(sent.last, UnitreeMotion(yaw: 0.5))
    }

    func testInputChangesDuringSendDoNotStartAnotherSender() async {
        var sent: [UnitreeMotion] = []
        var resume: CheckedContinuation<Void, Never>?
        let driver = UnitreeMotionDriver { value in
            sent.append(value)
            if sent.count == 1 { await withCheckedContinuation { resume = $0 } }
        }
        driver.enabled = true
        driver.update(.keys([13]), direction: "Keyboard")
        let first = Task { await driver.step(now: Date(timeIntervalSince1970: 1)) }
        while resume == nil { await Task.yield() }
        driver.update(.keys([13, 12]), direction: "Keyboard")
        driver.update(.keys([13, 12, 2]), direction: "Keyboard")
        XCTAssertEqual(sent.count, 1)
        resume?.resume()
        await first.value
        await driver.step(now: Date(timeIntervalSince1970: 1.2))
        XCTAssertEqual(sent, [UnitreeMotion(forward: 0.6), UnitreeMotion(forward: 0.6, lateral: -0.3, yaw: 0.5)])
    }

    func testTransportFailureStillDisablesAndAttemptsStop() async {
        var sent: [UnitreeMotion] = []
        let driver = UnitreeMotionDriver { value in
            sent.append(value)
            if value != UnitreeMotion() { throw WendyError(message: "Disconnected") }
        }
        driver.enabled = true
        driver.update(.keys([12]), direction: "Keyboard")
        await driver.step()
        XCTAssertFalse(driver.enabled)
        await driver.step()
        XCTAssertEqual(sent, [UnitreeMotion(yaw: 0.5), UnitreeMotion()])
        XCTAssertEqual(driver.error, "Disconnected")
    }
}

@MainActor
final class UnitreeKeyboardFocusTests: XCTestCase {
    func testOnlyControlWindowFocusLossClearsHeldKeys() {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 100, height: 100), styleMask: [.titled], backing: .buffered, defer: false)
        let otherWindow = NSWindow(contentRect: .zero, styleMask: [.titled], backing: .buffered, defer: false)
        let view = UnitreeKeyboard.KeyView()
        window.contentView = view
        defer { view.removeMonitor() }
        view.enabled = true
        view.keys = [13, 12]
        var suspensions = 0
        view.suspend = { suspensions += 1 }
        NotificationCenter.default.post(name: NSWindow.didResignKeyNotification, object: otherWindow)
        XCTAssertEqual(suspensions, 0)
        XCTAssertEqual(view.keys, [13, 12])
        NotificationCenter.default.post(name: NSWindow.didResignKeyNotification, object: window)
        XCTAssertEqual(suspensions, 1)
        XCTAssertTrue(view.keys.isEmpty)
        XCTAssertTrue(view.enabled)
    }
}
