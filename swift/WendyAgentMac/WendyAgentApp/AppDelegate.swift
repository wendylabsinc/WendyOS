import AppKit
import OSLog
import WendyAgentCore

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier!,
        category: "AppDelegate"
    )
    private let wendyAgent = WendyAgent()
    private var statusMenuController: StatusMenuController!

    func applicationDidFinishLaunching(_ notification: Notification) {
        let statusMenuController = StatusMenuController(wendyAgent: self.wendyAgent)
        self.statusMenuController = statusMenuController

        Task { @MainActor [weak self] in
            guard let self else { return }

            do {
                try await self.wendyAgent.start()
            } catch {
                self.logger.error("Failed to start WendyAgent: \(String(describing: error), privacy: .public)")
            }
        }
    }
}
