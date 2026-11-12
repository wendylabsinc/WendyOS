import Synchronization

/// A test-owned monotonic instant shared with the update-lock actor.
final class AgentUpdateTestClock: Sendable {
    private let instant = Mutex(ContinuousClock.now)

    func now() -> ContinuousClock.Instant {
        self.instant.withLock { $0 }
    }

    func advance(by duration: Duration) {
        self.instant.withLock { $0 = $0.advanced(by: duration) }
    }
}
