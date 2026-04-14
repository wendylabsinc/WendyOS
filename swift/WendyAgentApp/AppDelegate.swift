import AppKit
import WendyAgent

@MainActor
@main
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let agent = WendyAgent()
    private var status: WendyAgentStatus = .idle
    private var statusObservation: WendyObservation?
    private var hasBootstrapped = false
    private var isQuitting = false
    private var statusMenuController: StatusMenuController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let statusMenuController = StatusMenuController(status: self.status)
        statusMenuController.setQuitHandler { [weak self] in
            self?.quitSelected()
        }
        self.statusMenuController = statusMenuController
        self.bootstrapIfNeeded()
    }

    private func bootstrapIfNeeded() {
        guard !self.hasBootstrapped else { return }
        self.hasBootstrapped = true

        Task {
            self.statusObservation = await self.agent.observeStatus { status in
                Task { @MainActor in
                    self.updateStatus(status)
                }
            }

            do {
                try await self.agent.start()
            } catch {
                // WendyAgent publishes failure state directly.
            }
        }
    }

    private func updateStatus(_ status: WendyAgentStatus) {
        self.status = status
        self.statusMenuController?.update(status: status)
    }

    private func quitSelected() {
        guard !self.isQuitting else { return }
        self.isQuitting = true

        Task {
            await self.cancelStatusObservation()
            await self.agent.stop()
            NSApplication.shared.terminate(nil)
        }
    }

    private func cancelStatusObservation() async {
        guard let statusObservation = self.statusObservation else { return }
        self.statusObservation = nil
        await statusObservation.cancel()
    }
}
