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
    private let wendyAgent = WendyAgent()
    private let welcomeAndPermissions = WelcomeAndPermissions()
    private var statusMenuController: StatusMenuController?
    private var welcomeAndPermissionsWindow: NSWindow?
    // HACK: As an LSUIElement/accessory app, macOS sometimes restores the previously active app
    // after dismissing TCC permission prompts, which leaves this window behind other apps.
    // We paper over that race by retrying activation/fronting a few times.
    // Real fix: make onboarding/permissions run in a regular foreground app instead.
    // See WDY-930: https://linear.app/wendylabsinc/issue/WDY-930/explore-more-packaging-and-process-architecture-options-for-wendy-on
    private var welcomeAndPermissionsPresentationTask: Task<Void, Never>?
    private var isQuitting = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        self.welcomeAndPermissions.configureLaunchAtLoginOnStartup()

        self.statusMenuController = StatusMenuController(
            wendyAgent: self.wendyAgent,
            delegate: self
        )

        Task { @MainActor [weak self] in
            guard let self else { return }

            do {
                try await self.wendyAgent.start()
            } catch {
                self.logger.error(
                    "Failed to start WendyAgent: \(String(describing: error), privacy: .public)"
                )
            }
        }

        if self.welcomeAndPermissions.shouldShowWelcomeAndPermissions {
            self.showWelcomeAndPermissionsWindow()
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

    func statusMenuControllerDidSelectQuit(_ controller: StatusMenuController) {
        guard !self.isQuitting else { return }
        self.isQuitting = true

        Task { @MainActor [weak self] in
            guard let self else { return }

            await self.statusMenuController?.invalidate()
            await self.wendyAgent.stop()
            NSApplication.shared.terminate(nil)
        }
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
        self.welcomeAndPermissionsPresentationTask = Task { @MainActor [weak self] in
            // HACK: A single activate/orderFront call is racy here because the system permission
            // dialog may finish restoring the previously active app after our first attempt.
            // Retry a few times to keep the welcome window visible until WDY-930 is addressed by
            // moving this flow into a regular foreground app.
            let delays: [UInt64] = [150_000_000, 350_000_000, 750_000_000]

            for delay in delays {
                try? await Task.sleep(nanoseconds: delay)

                guard !Task.isCancelled,
                    let self,
                    let window = self.welcomeAndPermissionsWindow
                else {
                    return
                }

                self.presentWelcomeAndPermissionsWindow(window)
            }

            guard let self else { return }
            self.welcomeAndPermissionsPresentationTask = nil
        }
    }
}
