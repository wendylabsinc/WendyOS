import SwiftUI
import XCTest
@testable import WendyDesktop

@MainActor
final class LayoutRegressionTests: XCTestCase {
    override func setUp() {
        super.setUp()
        NSApplication.shared.accessibilitySetValue(true, forAttribute: NSAccessibility.Attribute(rawValue: "AXEnhancedUserInterface"))
    }

    private func attribute(_ element: NSObject, _ name: String) -> Any? {
        guard element.responds(to: NSSelectorFromString(name)) else { return nil }
        return element.value(forKey: name)
    }

    private func descendants(_ element: Any) -> [NSObject] {
        guard let accessible = element as? NSObject else { return [] }
        return [accessible] + ((attribute(accessible, "accessibilityChildren") as? [Any]) ?? []).flatMap(descendants)
    }

    private func label(_ element: NSObject) -> String? {
        attribute(element, "accessibilityLabel") as? String
            ?? attribute(element, "accessibilityTitle") as? String
            ?? attribute(element, "accessibilityValue") as? String
    }

    private func frame(_ element: NSObject) -> NSRect {
        (attribute(element, "accessibilityFrame") as? NSValue)?.rectValue ?? .zero
    }

    func testOverflowMenuKeepsFullHitArea() async throws {
        let host = NSHostingView(rootView: OverflowMenu {
            Button("Inspect") {}
        }.accessibilityLabel("Regression actions").padding(20))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 84, height: 84),
                              styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        window.orderFront(nil)
        defer { window.orderOut(nil); window.contentView = nil }
        try await Task.sleep(for: .milliseconds(100))
        host.layoutSubtreeIfNeeded()
        XCTAssertEqual(host.fittingSize.width, 84, accuracy: 1)
        XCTAssertEqual(host.fittingSize.height, 84, accuracy: 1)
        // SwiftUI exposes the menu as the hosting view's sole accessibility child.
        let button = try XCTUnwrap(descendants(host).dropFirst().first)
        let role = attribute(button, "accessibilityRole") as? String
        XCTAssertTrue(["AXButton", "AXMenuButton", "AXPopUpButton"].contains(role ?? ""), "Unexpected menu role: \(role ?? "nil")")
        XCTAssertGreaterThanOrEqual(frame(button).width, 44)
        XCTAssertGreaterThanOrEqual(frame(button).height, 44)
    }

    func testDeviceEmptyStateStaysCenteredWhenWindowResizes() async throws {
        let host = NSHostingView(rootView: ContentView().environmentObject(AppModel()))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1000, height: 700),
                              styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.contentView = host
        window.orderFront(nil)
        defer { window.orderOut(nil); window.contentView = nil }
        for size in [NSSize(width: 1000, height: 700), NSSize(width: 1400, height: 1000)] {
            window.setContentSize(size)
            try await Task.sleep(for: .milliseconds(100))
            host.layoutSubtreeIfNeeded()
            let elements = descendants(host)
            func element(_ text: String) throws -> NSObject {
                try XCTUnwrap(elements.first { label($0) == text }, "Missing \(text)")
            }
            let title = frame(try element("Your next device starts here"))
            let description = frame(try element("Power on a nearby device, or sign in to see your organization’s devices."))
            let install = frame(try element("Install Wendy on new hardware"))
            let subtitle = frame(try element("Pick a device and see what’s happening."))
            let header = frame(try element("Your devices"))
            let empty = title.union(description).union(install)
            let content = window.convertToScreen(host.convert(host.bounds, to: nil))
            // The landing page has 32 pt padding and 24 pt after its heading.
            // Center the full icon/text/action group in that remaining space.
            XCTAssertEqual(empty.midX, (header.minX + content.maxX - 32) / 2, accuracy: 3)
            XCTAssertEqual(empty.midY, (subtitle.minY - 24 + content.minY + 32) / 2, accuracy: 12)

        }
    }
}
