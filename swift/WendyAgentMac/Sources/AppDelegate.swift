import AppKit
import OSLog
import SwiftUI
import WendyAgentCore

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate,
    StatusMenuControllerDelegate
{
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier!,
        category: "AppDelegate"
    )
    private let networkAccessPreferences = NetworkAccessPreferences()
    private let networkAccessIsManaged = WendyAgentEnvironment.host != nil
    private var changingNetworkAccess = false
    private var networkAccessChangeTask: Task<Void, Never>?
    private lazy var wendyAgent: WendyAgent = {
        var configuration = WendyAgentConfiguration.default
        if WendyAgentEnvironment.host == nil,
            self.networkAccessPreferences.allowsNetworkAccess
        {
            configuration.host = "::"
        }
        return WendyAgent(configuration: configuration)
    }()
    private let localBuildService = WendyRuntimeVM()
    // DISABLED: Local Build Service is hidden while the BuildKit experience remains unfinished.
    private let localBuildServiceIsEnabled = false
    private let meshVPN = MeshVPNController.shared
    // DISABLED: Wendy Mesh is hidden until its sign-in and account experience is ready.
    private let meshVPNIsEnabled = false
    private var meshVPNIsAvailable: Bool {
        self.meshVPNIsEnabled && MeshSystemExtensionInstaller.isBundled
    }
    private let launchAgent = LaunchAgentSupervision()
    private lazy var welcomeAndPermissions = WelcomeAndPermissions(launchAtLogin: self.launchAgent)
    private var statusMenuController: StatusMenuController?
    private var welcomeAndPermissionsWindow: NSWindow?
    // HACK: As an LSUIElement/accessory app, macOS sometimes restores the previously active app
    // after dismissing TCC permission prompts, which leaves this window behind other apps.
    // We paper over that race by retrying activation/fronting a few times.
    // Real fix: make onboarding/permissions run in a regular foreground app instead.
    // See WDY-930: https://linear.app/wendylabsinc/issue/WDY-930/explore-more-packaging-and-process-architecture-options-for-wendy-on
    private var welcomeAndPermissionsPresentationTask: Task<Void, any Error>?
    private var isQuitting = false
    /// Reopens the welcome window after a relaunch that the window's own
    /// launch-at-login toggle caused.
    private static let showWelcomeAfterRelaunchKey = "showWelcomeAndPermissionsAfterRelaunch"

    func applicationDidFinishLaunching(_ notification: Notification) {
        self.launchAgent.onRelaunchRequired = { [weak self] in
            self?.relaunchForLaunchAgentChange()
        }

        Task {
            switch await self.launchAgent.prepareForLaunch() {
            case .run:
                break
            case .quit:
                NSApplication.shared.terminate(nil)
                return
            case .relaunch:
                self.relaunch()
                return
            }

            // Only the instance that runs the agent resolves the choice, so a
            // launch that quits or relaunches does not consume the prompt.
            let shouldAskForNetworkAccess =
                !self.networkAccessIsManaged
                && self.networkAccessPreferences.prepareForStartup(
                    existingInstallation: NetworkAccessPreferences.existingInstallation
                )

            let defaults = UserDefaults.standard
            let showWelcomeAfterRelaunch = defaults.bool(forKey: Self.showWelcomeAfterRelaunchKey)
            defaults.removeObject(forKey: Self.showWelcomeAfterRelaunchKey)
            if showWelcomeAfterRelaunch
                || self.welcomeAndPermissions.shouldShowWelcomeAndPermissions
            {
                self.showWelcomeAndPermissionsWindow()
            }

            self.statusMenuController = await StatusMenuController(
                wendyAgent: self.wendyAgent,
                localBuildService: self.localBuildService,
                localBuildServiceIsAvailable: self.localBuildServiceIsEnabled,
                meshVPN: self.meshVPN,
                meshVPNIsAvailable: self.meshVPNIsAvailable,
                networkAccessIsManaged: self.networkAccessIsManaged,
                delegate: self
            )

            if !self.localBuildServiceIsEnabled {
                // DISABLED: Stop any process-local BuildKit runtime so the hidden service cannot
                // remain active without visible controls.
                await self.localBuildService.stop()
            }

            if self.meshVPNIsAvailable {
                await self.meshVPN.connectAutomatically()
            } else {
                // DISABLED: Clear prior opt-ins and stop existing tunnels so hidden Mesh cannot
                // remain active or reconnect in the background.
                await self.meshVPN.disable()
            }

            // Registered before start() so the services the agent builds at
            // startup capture it. A self-update must end this process without
            // running the ordinary Quit teardown: deployed apps should survive
            // and be adopted by the replacement agent, and draining the gRPC
            // server here can wait on the update RPC that triggered the quit.
            await self.wendyAgent.setAgentTerminationHandler { [weak self] in
                await self?.performUpdateQuit()
            }

            do {
                try await self.wendyAgent.start()
                if shouldAskForNetworkAccess {
                    self.chooseInitialNetworkAccess()
                }
            } catch {
                self.logger.error(
                    "Failed to start WendyAgent: \(String(describing: error), privacy: .public)"
                )
            }
        }
    }

    private func chooseInitialNetworkAccess() {
        guard !self.networkAccessIsManaged else { return }

        let alert = NSAlert()
        alert.messageText = "Who should be able to connect to Wendy Agent?"
        alert.informativeText =
            "Only this Mac keeps access local to your computer. "
            + "Allow Other Devices lets other computers discover and connect to this agent. "
            + "Before enrollment, reachable computers may deploy and run apps. "
            + "You can change this later in the Wendy Agent menu."
        alert.addButton(withTitle: "Only This Mac")
        alert.addButton(withTitle: "Allow Other Devices")
        alert.buttons.first?.keyEquivalent = "\r"
        alert.buttons.last?.keyEquivalent = ""
        NSApplication.shared.activate(ignoringOtherApps: true)
        let response = alert.runModal()
        self.networkAccessPreferences.markPromptAnswered()
        if response == .alertSecondButtonReturn, let controller = self.statusMenuController {
            self.statusMenuController(controller, didSetNetworkAccessAllowed: true)
        }
    }

    func statusMenuControllerDidSelectAbout(_ controller: StatusMenuController) {
        NSApplication.shared.activate(ignoringOtherApps: true)
        NSApplication.shared.orderFrontStandardAboutPanel(options: [
            .applicationName: AppDisplayName.current
        ])
    }

    func statusMenuControllerDidSelectWelcomeAndPermissions(_ controller: StatusMenuController) {
        self.showWelcomeAndPermissionsWindow()
    }

    func statusMenuController(
        _ controller: StatusMenuController,
        didSetNetworkAccessAllowed allowed: Bool
    ) {
        guard !self.networkAccessIsManaged, !self.changingNetworkAccess, !self.isQuitting else {
            return
        }
        self.changingNetworkAccess = true
        controller.updateNetworkAccess(isChanging: true)
        // Persist a revocation before doing any asynchronous shutdown work.
        if !allowed { self.networkAccessPreferences.allowsNetworkAccess = false }

        self.networkAccessChangeTask = Task {
            defer {
                self.changingNetworkAccess = false
                self.networkAccessChangeTask = nil
                controller.updateNetworkAccess(isChanging: false)
            }
            do {
                try await self.wendyAgent.setNetworkAccess(allowed: allowed)
                self.networkAccessPreferences.allowsNetworkAccess = allowed
                self.networkAccessPreferences.markPromptAnswered()
                controller.updateNetworkAccess(allowed: allowed)
            } catch {
                let configuration = await self.wendyAgent.configuration
                controller.updateNetworkAccess(allowed: configuration.allowsNetworkAccess)
                self.logger.error(
                    "Failed to change network access: \(String(describing: error), privacy: .public)"
                )
                let alert = NSAlert()
                alert.messageText = "Could not change network access"
                alert.informativeText = String(describing: error)
                NSApplication.shared.activate(ignoringOtherApps: true)
                alert.runModal()
            }
        }
    }

    func statusMenuController(
        _ controller: StatusMenuController,
        didSetLocalBuildServiceEnabled enabled: Bool
    ) {
        guard self.localBuildServiceIsEnabled else { return }

        Task {
            if enabled {
                await self.localBuildService.start()
            } else {
                await self.localBuildService.stop()
            }
        }
    }

    func statusMenuController(
        _ controller: StatusMenuController,
        didSetMeshVPNEnabled enabled: Bool
    ) {
        guard self.meshVPNIsAvailable else { return }

        Task {
            if enabled {
                await self.meshVPN.connect()
            } else {
                await self.meshVPN.disable()
            }
        }
    }

    func statusMenuControllerDidSelectNetworkExtensionSettings(
        _ controller: StatusMenuController
    ) {
        self.openNetworkExtensionSettings()
    }

    func statusMenuControllerDidSelectQuit(_ controller: StatusMenuController) {
        self.performQuit()
    }

    /// Shuts the agent down and terminates the app after an explicit user Quit.
    private func performQuit() {
        guard !self.isQuitting else { return }
        self.isQuitting = true

        Task {
            await self.networkAccessChangeTask?.value
            await self.statusMenuController?.invalidate()
            await self.wendyAgent.stop()
            await self.localBuildService.stop()
            NSApplication.shared.terminate(nil)
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        self.localBuildService.requestStop()
    }

    /// Ends only the agent app after a committed self-update. The detached
    /// relaunch watcher opens the freshly-installed bundle once this PID exits.
    private func performUpdateQuit() {
        NSApplication.shared.terminate(nil)
    }

    private func relaunchForLaunchAgentChange() {
        if self.welcomeAndPermissionsWindow != nil {
            UserDefaults.standard.set(true, forKey: Self.showWelcomeAfterRelaunchKey)
        }
        self.relaunch()
    }

    /// Reopens this bundle outside launchd once this process has exited. Like a
    /// self-update, it skips the Quit teardown so deployed apps keep running
    /// and are adopted by the new instance.
    private func relaunch() {
        do {
            try AgentRelauncher(terminate: nil).scheduleRelaunch(of: Bundle.main.bundleURL)
        } catch {
            self.logger.error(
                "Failed to schedule Wendy Agent relaunch: \(String(describing: error), privacy: .public)"
            )
            return
        }
        self.performUpdateQuit()
    }

    private func openNetworkExtensionSettings() {
        guard
            let url = URL(
                string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"
            )
        else {
            return
        }
        NSWorkspace.shared.open(url)
    }

    func windowWillClose(_ notification: Notification) {
        guard let window = notification.object as? NSWindow,
            window === self.welcomeAndPermissionsWindow
        else {
            return
        }

        self.welcomeAndPermissionsPresentationTask?.cancel()
        self.welcomeAndPermissionsPresentationTask = nil
        self.welcomeAndPermissionsWindow = nil
    }

    private func makeWelcomeAndPermissionsWindow() -> NSWindow {
        let rootView = WelcomeAndPermissionsView(
            welcomeAndPermissions: self.welcomeAndPermissions,
            onPermissionRequestCompleted: { [weak self] in
                guard self?.welcomeAndPermissionsWindow != nil else { return }
                self?.reassertWelcomeAndPermissionsWindowPresentation()
            }
        )
        let hostingController = NSHostingController(rootView: rootView)

        let welcomeAndPermissionsWindow = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 620, height: 500),
            styleMask: [.titled, .closable],
            backing: .buffered,
            defer: false
        )

        welcomeAndPermissionsWindow.contentViewController = hostingController
        welcomeAndPermissionsWindow.delegate = self
        welcomeAndPermissionsWindow.isReleasedWhenClosed = false

        if let closeButton = welcomeAndPermissionsWindow.standardWindowButton(.closeButton) {
            closeButton.keyEquivalent = "w"
            closeButton.keyEquivalentModifierMask = [.command]
        }

        let contentView = welcomeAndPermissionsWindow.contentView!

        contentView.layoutSubtreeIfNeeded()
        let fittingSize = contentView.fittingSize
        let contentSize = NSSize(
            width: max(620, fittingSize.width),
            height: max(320, fittingSize.height)
        )
        welcomeAndPermissionsWindow.setContentSize(contentSize)

        return welcomeAndPermissionsWindow
    }

    private func showWelcomeAndPermissionsWindow() {
        self.welcomeAndPermissions.prepareForPresentation()

        if let welcomeAndPermissionsWindow = self.welcomeAndPermissionsWindow {
            self.presentWelcomeAndPermissionsWindow(welcomeAndPermissionsWindow)
            return
        }

        let welcomeAndPermissionsWindow = self.makeWelcomeAndPermissionsWindow()
        self.welcomeAndPermissionsWindow = welcomeAndPermissionsWindow
        welcomeAndPermissionsWindow.center()
        welcomeAndPermissionsWindow.setFrameAutosaveName("WelcomeAndPermissionsWindow")
        self.presentWelcomeAndPermissionsWindow(welcomeAndPermissionsWindow)
    }

    private func presentWelcomeAndPermissionsWindow(_ window: NSWindow) {
        NSApplication.shared.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
    }

    private func reassertWelcomeAndPermissionsWindowPresentation() {
        self.showWelcomeAndPermissionsWindow()

        self.welcomeAndPermissionsPresentationTask?.cancel()
        self.welcomeAndPermissionsPresentationTask = Task {
            // HACK: A single activate/orderFront call is racy here because the system permission
            // dialog may finish restoring the previously active app after our first attempt.
            // Retry a few times to keep the welcome window visible until WDY-930 is addressed by
            // moving this flow into a regular foreground app.
            let delays: [UInt64] = [150_000_000, 350_000_000, 750_000_000]

            for delay in delays {
                try await Task.sleep(nanoseconds: delay)

                guard
                    !Task.isCancelled,
                    let window = self.welcomeAndPermissionsWindow
                else {
                    return
                }

                self.presentWelcomeAndPermissionsWindow(window)
            }

            self.welcomeAndPermissionsPresentationTask = nil
        }
    }
}

extension LaunchAgentSupervision: LaunchAtLoginSetting {}
