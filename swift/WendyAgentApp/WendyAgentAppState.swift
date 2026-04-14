import AppKit
import Combine
import WendyAgent

@MainActor
final class WendyAgentAppState: ObservableObject {
    @Published private(set) var status: WendyAgentStatus = .idle

    private let agent: WendyAgent
    private var observationTask: Task<Void, Never>?
    private var startupTask: Task<Void, Never>?
    private var quitTask: Task<Void, Never>?

    init(agent: WendyAgent = WendyAgent()) {
        self.agent = agent
        self.observationTask = Task {
            let updates = await agent.statusUpdates()
            for await status in updates {
                self.status = status
            }
        }
    }

    deinit {
        self.observationTask?.cancel()
    }

    func startIfNeeded() {
        guard self.startupTask == nil else { return }

        self.startupTask = Task {
            do {
                try await self.agent.start()
            } catch {
                // WendyAgent publishes failure state directly.
            }
        }
    }

    func quit() {
        guard self.quitTask == nil else { return }

        self.quitTask = Task {
            await self.agent.stop()
            NSApplication.shared.terminate(nil)
        }
    }
}
