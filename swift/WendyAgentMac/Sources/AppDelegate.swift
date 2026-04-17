import AppKit
import OSLog
import SwiftUI
import WendyAgentCore

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, StatusMenuControllerDelegate {
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier!,
        category: "AppDelegate"
    )
    private let wendyAgent = WendyAgent()
    private let onboarding = Onboarding()
    private var statusMenuController: StatusMenuController?
    private var onboardingWindow: NSWindow?
    private var isQuitting = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        self.statusMenuController = StatusMenuController(
            wendyAgent: self.wendyAgent,
            delegate: self
        )

        Task { @MainActor [weak self] in
            guard let self else { return }

            do {
                try await self.wendyAgent.start()
            } catch {
                self.logger.error("Failed to start WendyAgent: \(String(describing: error), privacy: .public)")
            }
        }

        if self.onboarding.shouldShowOnboarding {
            self.showOnboardingWindow()
        }
    }

    func statusMenuControllerDidSelectWelcomeAndPermissions(_ controller: StatusMenuController) {
        self.showOnboardingWindow()
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
              window === self.onboardingWindow
        else {
            return
        }

        self.onboardingWindow = nil
    }

    private func showOnboardingWindow() {
        self.onboarding.prepareForPresentation()

        if let onboardingWindow = self.onboardingWindow {
            self.sizeOnboardingWindowToFit(onboardingWindow)
            NSApplication.shared.activate(ignoringOtherApps: true)
            onboardingWindow.makeKeyAndOrderFront(nil)
            onboardingWindow.center()
            return
        }

        let rootView = OnboardingView(onboarding: self.onboarding)
        let hostingController = NSHostingController(rootView: rootView)

        let onboardingWindow = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 620, height: 500),
            styleMask: [.titled, .closable],
            backing: .buffered,
            defer: false
        )

        onboardingWindow.title = "Welcome to \(AppDisplayName.current)"
        onboardingWindow.contentViewController = hostingController
        onboardingWindow.delegate = self
        onboardingWindow.isReleasedWhenClosed = false

        self.onboardingWindow = onboardingWindow

        self.sizeOnboardingWindowToFit(onboardingWindow)
        NSApplication.shared.activate(ignoringOtherApps: true)
        onboardingWindow.makeKeyAndOrderFront(nil)
        onboardingWindow.center()
    }

    private func sizeOnboardingWindowToFit(_ window: NSWindow) {
        guard let contentView = window.contentView else { return }

        contentView.layoutSubtreeIfNeeded()
        let fittingSize = contentView.fittingSize
        let contentSize = NSSize(
            width: max(620, fittingSize.width),
            height: max(320, fittingSize.height)
        )
        window.setContentSize(contentSize)
    }
}
