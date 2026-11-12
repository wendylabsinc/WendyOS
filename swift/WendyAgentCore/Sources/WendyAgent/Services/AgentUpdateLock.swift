/// Serializes agent updates across main-server rebuilds. Deliberately NOT
/// released after a successful commit — the process is about to exit, and a
/// retry must see "an update is already in progress".
///
/// Self-healing by design: an acquisition that never committed and is older
/// than `staleThreshold` may be stolen. grpc-swift's `ServerRPCExecutor`
/// writes the response's initial metadata *before* running the response
/// producer, so a client that aborts in that window leaves the lock held with
/// nothing left to run its `release()`. Without the steal, that single lost
/// release would fail every later update with `failedPrecondition` until
/// somebody restarts the app by hand.
actor AgentUpdateLock {
    /// How long an uncommitted acquisition stays authoritative. A genuine
    /// update session cannot come close: the payload is capped at 256 MiB and
    /// the verify/extract/swap steps are all bounded, so anything older is a
    /// leaked acquisition rather than a live update.
    static let defaultStaleThreshold: Duration = .seconds(30 * 60)

    private struct Holder {
        let acquiredAt: ContinuousClock.Instant
        var isCommitted: Bool
    }

    private let staleThreshold: Duration
    private let now: @Sendable () -> ContinuousClock.Instant
    private var holder: Holder?

    /// Tests can advance a monotonic clock without depending on task scheduling.
    init(
        staleThreshold: Duration = AgentUpdateLock.defaultStaleThreshold,
        now: @escaping @Sendable () -> ContinuousClock.Instant = { ContinuousClock.now }
    ) {
        self.staleThreshold = staleThreshold
        self.now = now
    }

    /// Attempts to acquire the lock. Returns `false` while it is held by a
    /// live acquisition — one that has already committed (the process is on
    /// its way out; stealing there could double-install over the bundle just
    /// written) or one younger than `staleThreshold`.
    func tryAcquire() -> Bool {
        let now = self.now()
        if let holder = self.holder {
            guard !holder.isCommitted, now - holder.acquiredAt >= self.staleThreshold else {
                return false
            }
        }
        self.holder = Holder(acquiredAt: now, isCommitted: false)
        return true
    }

    /// Pins the in-flight update as committed: the new bundle is installed and
    /// termination is about to be scheduled, so this lock must never be
    /// stolen no matter how long the shutdown takes.
    func markCommitted() {
        self.holder?.isCommitted = true
    }

    /// Releases the lock. A successful update commit must NOT call this —
    /// the process exits shortly afterward, so the lock only needs to be
    /// released on abandoned/failed attempts. See the type-level doc comment.
    func release() {
        self.holder = nil
    }
}
