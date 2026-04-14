import AppKit
import WendyAgent

@MainActor
final class StatusMenuController: NSObject {
    init(status: WendyAgentStatus) {
        self.currentStatus = status
        self.statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        super.init()

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

        button.image = NSImage(named: NSImage.Name("StatusIcon"))
        button.image?.isTemplate = true
        button.imagePosition = .imageOnly
        button.toolTip = "WendyAgent — \(self.currentStatus.menuTitle)"
    }

    private func makeStatusImage(for status: WendyAgentStatus) -> NSImage {
        let image = NSImage(size: NSSize(width: 10, height: 10))
        image.lockFocus()
        defer { image.unlockFocus() }

        status.menuStatusColor.setFill()
        NSBezierPath(ovalIn: NSRect(x: 1, y: 1, width: 8, height: 8)).fill()
        image.isTemplate = false
        return image
    }

    @objc
    private func quitSelected() {
        self.onQuit?()
    }
}
