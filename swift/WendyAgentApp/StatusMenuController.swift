import AppKit
import WendyAgent

@MainActor
final class StatusMenuController: NSObject {
    init(status: WendyAgentStatus) {
        self.currentStatus = status
        self.statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        super.init()

        self.statusItem.isVisible = true
        self.updateStatusButton()
        self.rebuildMenu()
    }

    func update(status: WendyAgentStatus) {
        self.currentStatus = status
        self.updateStatusButton()
        self.rebuildMenu()
    }

    func setQuitHandler(_ handler: @escaping () -> Void) {
        self.onQuit = handler
    }

    private let statusItem: NSStatusItem
    private var onQuit: (() -> Void)?
    private var currentStatus: WendyAgentStatus

    private func rebuildMenu() {
        let menu = NSMenu()
        menu.autoenablesItems = false

        let statusItem = NSMenuItem(
            title: self.currentStatus.menuTitle,
            action: nil,
            keyEquivalent: ""
        )
        statusItem.image = self.makeStatusImage(for: self.currentStatus)
        statusItem.isEnabled = false
        menu.addItem(statusItem)

        for detail in self.currentStatus.menuFailureDetails {
            let detailItem = NSMenuItem(title: detail, action: nil, keyEquivalent: "")
            detailItem.isEnabled = false
            menu.addItem(detailItem)
        }

        menu.addItem(.separator())

        let quitItem = NSMenuItem(
            title: "Quit WendyAgent",
            action: #selector(self.quitSelected),
            keyEquivalent: "q"
        )
        quitItem.target = self
        menu.addItem(quitItem)

        self.statusItem.menu = menu
    }

    private func updateStatusButton() {
        guard let button = self.statusItem.button else { return }

        let image = self.makeButtonImage()
        image?.isTemplate = true

        button.image = image
        button.title = image == nil ? "W" : ""
        button.imagePosition = image == nil ? .noImage : .imageOnly
        button.imageScaling = .scaleProportionallyDown
        button.toolTip = "WendyAgent — \(self.currentStatus.menuTitle)"
        button.setAccessibilityTitle("WendyAgent")
    }

    private func makeButtonImage() -> NSImage? {
        if let image = NSImage(named: NSImage.Name("StatusIcon"))?.copy() as? NSImage {
            return image
        }

        return NSImage(
            systemSymbolName: "diamond.fill",
            accessibilityDescription: "WendyAgent"
        )
    }

    private func makeStatusImage(for status: WendyAgentStatus) -> NSImage? {
        guard let image = NSImage(named: NSImage.Name(status.menuImageName))?.copy() as? NSImage else {
            return nil
        }

        image.isTemplate = false
        image.size = NSSize(width: 10, height: 10)
        return image
    }

    @objc
    private func quitSelected() {
        self.onQuit?()
    }
}
