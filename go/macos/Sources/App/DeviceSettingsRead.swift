import Foundation

/// Serialize bounded reads across a settings navigation push. Each visible
/// page owns its task: a new page waits for the cancelled menu read to drain,
/// then performs its own read instead of inheriting an empty loading state.
@MainActor
final class DeviceSettingsRead {
    private var reading = false
    private var waiters: [UUID: AsyncCallback<Void>] = [:]

    func run(operation: @escaping @MainActor @Sendable () async -> Void) async {
        guard !Task.isCancelled else { return }
        while reading {
            let id = UUID()
            let finished = AsyncCallback<Void>()
            waiters[id] = finished
            _ = try? await finished.value()
            waiters[id] = nil
            guard !Task.isCancelled else { return }
        }
        reading = true
        defer {
            reading = false
            let pending = Array(waiters.values)
            waiters.removeAll()
            for waiter in pending { waiter.finish(.success(())) }
        }
        await operation()
    }
}
