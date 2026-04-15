import AppKit
import WendyAgentCore

@MainActor
final class StatusMenuController: NSObject {
    let wendyAgent: WendyAgent

    init(wendyAgent: WendyAgent) {
        self.wendyAgent = wendyAgent
        self.currentStatus = wendyAgent.status
        self.statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        super.init()

        self.statusObservation = self.wendyAgent.observeStatus { @MainActor [weak self] status in
            self?.update(status: status)
        }

        self.statusItem.isVisible = true
        self.updateStatusButton()
        self.rebuildMenu()
    }

    private let statusItem: NSStatusItem
    private var currentStatus: WendyAgentStatus
    private var statusObservation: WendyObservation?
    private var isQuitting = false

    private func update(status: WendyAgentStatus) {
        self.currentStatus = status
        self.updateStatusButton()
        self.rebuildMenu()
    }

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
        return image
    }

    @objc
    private func quitSelected() {
        guard !self.isQuitting else { return }
        self.isQuitting = true

        Task { @MainActor in
            await self.cancelStatusObservation()
            await self.wendyAgent.stop()
            NSApplication.shared.terminate(nil)
        }
    }

    private func cancelStatusObservation() async {
        guard let statusObservation = self.statusObservation else { return }
        self.statusObservation = nil
        await statusObservation.cancel()
    }
}
