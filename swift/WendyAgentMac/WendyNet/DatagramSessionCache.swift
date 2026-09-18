import Foundation
import WendyAgentCore
import os

/// One `CloudDatagramSession` per asset, created lazily, dropped on close so the
/// next use reopens. Each provider (proxy / packet-tunnel) owns its own cache;
/// the two resulting sessions per device are independent by design — session
/// state (flow-id space, echo handlers) is scoped to whichever provider created it.
actor DatagramSessionCache {
    // Memoized as a Task, not a resolved session: the *first* concurrent caller for a given
    // assetID stores its in-flight open Task synchronously (no `await` between the `sessions
    // [assetID]` check and this store — see below), so every later caller for that assetID,
    // including ones racing in before the open completes, awaits the *same* Task and gets the
    // same session (or the same failure). Without this, two datagrams to a newly-seen device
    // arriving in the same readDatagrams batch could each call `CloudDatagramSession.open`
    // before either finishes, leaking one of the two sessions forever.
    private var sessionTasks: [Int32: Task<WendyCloudDatagramSession, any Error>] = [:]
    private let flowIDs = FlowIDCounter()

    func session(
        for assetID: Int32,
        config: ExtensionConfig
    ) async throws
        -> WendyCloudDatagramSession
    {
        if let existing = sessionTasks[assetID] {
            return try await existing.value
        }
        // Everything above and this store is synchronous (no suspension point), so this is the
        // one and only Task ever stored for `assetID` until it completes.
        let task = Task<WendyCloudDatagramSession, any Error> {
            try await WendyCloudDatagramSession.open(
                .init(
                    assetID: assetID,
                    cloudGRPC: config.cloudGRPC,
                    credentials: config.credentials
                )
            )
        }
        sessionTasks[assetID] = task
        do {
            let session = try await task.value
            await session.onClose { [weak self] in
                Task { await self?.dropSession(assetID: assetID) }
            }
            return session
        } catch {
            // Open failed: evict so the next call retries instead of replaying this error
            // forever against a memoized dead Task.
            dropSession(assetID: assetID)
            throw error
        }
    }

    private func dropSession(assetID: Int32) {
        sessionTasks[assetID] = nil
    }

    /// Allocates the next flow ID for this cache's `CloudDatagramSession`s. Flow IDs are unique
    /// per device session across every NE UDP flow that relays to that device — a session's
    /// `datagramHandlers` map is keyed globally by flow ID, so two independent NE flows handing
    /// out the same ID would silently clobber each other's handler registration.
    ///
    /// This is `nonisolated` and synchronous on purpose: `UDPFlowMap.id(for:)` needs to allocate
    /// a fresh ID (when one hasn't already been recorded for an endpoint) with no suspension
    /// point between checking its map and recording the new ID. Going through this actor's
    /// isolated state with an `await` would reopen exactly that race (two concurrent lookups for
    /// the same new endpoint could each miss the check and each allocate/store an ID, orphaning
    /// one handler registration). A plain lock-protected counter lets the allocation happen
    /// synchronously inside `UDPFlowMap`'s own atomic check-then-store.
    nonisolated func nextFlowID() -> UInt32 {
        flowIDs.next()
    }
}

/// Thread-safe monotonic counter backing `DatagramSessionCache.nextFlowID()`.
private final class FlowIDCounter: Sendable {
    private let counter = OSAllocatedUnfairLock<UInt32>(initialState: 0)

    func next() -> UInt32 {
        counter.withLock { value in
            value &+= 1
            return value
        }
    }
}
