import AppKit
import WendyAgentCore

@MainActor
protocol StatusMenuControllerDelegate: AnyObject {
    func statusMenuControllerDidSelectAbout(_ controller: StatusMenuController)
    func statusMenuControllerDidSelectWelcomeAndPermissions(_ controller: StatusMenuController)
    func statusMenuControllerDidSelectQuit(_ controller: StatusMenuController)
}

@MainActor
final class StatusMenuController: NSObject {
    let wendyAgent: WendyAgent

    init(
        wendyAgent: WendyAgent,
        delegate: StatusMenuControllerDelegate? = nil,
        bundle: Bundle = .main
    ) {
        self.wendyAgent = wendyAgent
        self.delegate = delegate
        self.bundleDisplayName = AppDisplayName.resolve(from: bundle)
        self.currentStatus = wendyAgent.status
        self.currentApps = wendyAgent.apps
        self.statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        self.menu = NSMenu()
        super.init()

        self.statusObservation = self.wendyAgent.observeStatus { @MainActor [weak self] status in
            self?.update(status: status)
        }
        self.appsObservation = self.wendyAgent.observeApps { @MainActor [weak self] apps in
            self?.update(apps: apps)
        }

        self.menu.autoenablesItems = false
        self.statusItem.menu = self.menu
        self.statusItem.isVisible = true
        self.updateStatusButton()
        self.rebuildMenu()
    }

    weak var delegate: StatusMenuControllerDelegate?

    private let bundleDisplayName: String
    private let statusItem: NSStatusItem
    private let menu: NSMenu
    private var currentStatus: WendyAgentStatus
    private var currentApps: [WendyAppInfo]
    private var statusObservation: WendyObservation?
    private var appsObservation: WendyObservation?

    private var runningApps: [WendyAppInfo] {
        self.currentApps
            .filter { $0.status == .running }
            .sorted { $0.id < $1.id }
    }

    private func update(status: WendyAgentStatus) {
        self.currentStatus = status
        self.updateStatusButton()
        self.rebuildMenu()
    }

    private func update(apps: [WendyAppInfo]) {
        self.currentApps = apps
        self.rebuildMenu()
    }

    private func rebuildMenu() {
        self.menu.removeAllItems()

        let aboutItem = NSMenuItem(
            title: "About \(self.bundleDisplayName)",
            action: #selector(self.aboutSelected),
            keyEquivalent: ""
        )
        aboutItem.target = self
        self.menu.addItem(aboutItem)

        let welcomeItem = NSMenuItem(
            title: "Welcome & Permissions…",
            action: #selector(self.welcomeAndPermissionsSelected),
            keyEquivalent: ""
        )
        welcomeItem.target = self
        self.menu.addItem(welcomeItem)

        self.menu.addItem(.separator())

        let statusItem = self.makeDisabledMenuItem(title: self.currentStatus.menuTitle)
        statusItem.image = self.makeStatusImage(for: self.currentStatus)
        self.menu.addItem(statusItem)

        for detail in self.currentStatus.menuFailureDetails {
            self.menu.addItem(self.makeDisabledMenuItem(title: detail))
        }

        self.addRunningAppsSection()
        self.menu.addItem(.separator())

        let quitItem = NSMenuItem(
            title: "Quit \(self.bundleDisplayName)",
            action: #selector(self.quitSelected),
            keyEquivalent: "q"
        )
        quitItem.target = self
        self.menu.addItem(quitItem)
    }

    private func addRunningAppsSection() {
        let runningApps = self.runningApps

        for app in runningApps {
            let appItem = NSMenuItem(title: app.id, action: nil, keyEquivalent: "")
            appItem.submenu = self.makeAppSubmenu(for: app)
            self.menu.addItem(appItem)
        }
    }

    private func makeAppSubmenu(for app: WendyAppInfo) -> NSMenu {
        let submenu = NSMenu(title: app.id)
        let details = [
            "ID: \(app.id)",
            "Kind: \(self.displayName(for: app.kind))",
            "Status: \(self.displayName(for: app.status))",
            "PID: \(app.pid.map(String.init) ?? "Unknown")",
        ]

        for detail in details {
            submenu.addItem(self.makeDisabledMenuItem(title: detail))
        }

        return submenu
    }

    private func makeDisabledMenuItem(title: String) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        return item
    }

    private func displayName(for kind: WendyAppInfo.Kind) -> String {
        switch kind {
        case .native:
            return "Native"
        case .container:
            return "Container"
        }
    }

    private func displayName(for status: WendyAppInfo.Status) -> String {
        switch status {
        case .stopped:
            return "Stopped"
        case .running:
            return "Running"
        }
    }

    private func updateStatusButton() {
        guard let button = self.statusItem.button else { return }

        let image = self.makeButtonImage(for: self.currentStatus)
        image?.isTemplate = true

        button.image = image
        button.title = self.buttonTitle(for: self.currentStatus, image: image)
        button.imagePosition = self.buttonImagePosition(for: self.currentStatus, image: image)
        button.imageScaling = .scaleProportionallyDown
        button.toolTip = "\(self.bundleDisplayName) — \(self.currentStatus.menuTitle)"
        button.setAccessibilityTitle(self.bundleDisplayName)
    }

    private func buttonTitle(for status: WendyAgentStatus, image: NSImage?) -> String {
        if case .failed = status {
            return "!"
        }

        return image == nil ? "W" : ""
    }

    private func buttonImagePosition(
        for status: WendyAgentStatus,
        image: NSImage?
    ) -> NSControl.ImagePosition {
        guard image != nil else {
            return .noImage
        }

        if case .failed = status {
            return .imageLeading
        }

        return .imageOnly
    }

    private func makeButtonImage(for status: WendyAgentStatus) -> NSImage? {
        if let image = NSImage(named: NSImage.Name("StatusIcon"))?.copy() as? NSImage {
            return image
        }

        return NSImage(
            systemSymbolName: "diamond.fill",
            accessibilityDescription: self.bundleDisplayName
        )
    }

    private func makeStatusImage(for status: WendyAgentStatus) -> NSImage? {
        guard let image = NSImage(named: NSImage.Name(status.menuImageName))?.copy() as? NSImage
        else {
            return nil
        }

        image.isTemplate = false
        return image
    }

    func invalidate() async {
        await self.cancelObservations()
    }

    @objc
    private func aboutSelected() {
        self.delegate?.statusMenuControllerDidSelectAbout(self)
    }

    @objc
    private func welcomeAndPermissionsSelected() {
        self.delegate?.statusMenuControllerDidSelectWelcomeAndPermissions(self)
    }

    @objc
    private func quitSelected() {
        self.delegate?.statusMenuControllerDidSelectQuit(self)
    }

    private func cancelObservations() async {
        await self.cancelStatusObservation()
        await self.cancelAppsObservation()
    }

    private func cancelStatusObservation() async {
        guard let statusObservation = self.statusObservation else { return }
        self.statusObservation = nil
        await statusObservation.cancel()
    }

    private func cancelAppsObservation() async {
        guard let appsObservation = self.appsObservation else { return }
        self.appsObservation = nil
        await appsObservation.cancel()
    }
}
