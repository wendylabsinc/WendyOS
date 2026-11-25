import Foundation

/// UI events run as children of the visible workspace's task. Cancellation
/// joins outstanding work, including requests with an uncertain server outcome.
@MainActor
final class TwinActions {
    typealias Action = @MainActor @Sendable () async -> Void
    private var pending: [Action] = []
    private var continuation: AsyncStream<Action>.Continuation?
    private var generation = 0

    func submit(_ action: @escaping Action) {
        if let continuation, case .enqueued = continuation.yield(action) { return }
        pending.append(action)
    }

    func run() async {
        guard !Task.isCancelled else { return }
        continuation?.finish()
        generation += 1
        let current = generation
        let stream = AsyncStream<Action>.makeStream()
        continuation = stream.continuation
        for action in pending { stream.continuation.yield(action) }
        pending.removeAll()
        defer {
            stream.continuation.finish()
            if generation == current { continuation = nil }
        }
        await withDiscardingTaskGroup { group in
            defer { group.cancelAll() }
            for await action in stream.stream {
                guard !Task.isCancelled else { break }
                group.addTask {
                    guard !Task.isCancelled else { return }
                    await action()
                }
            }
        }
    }
}
