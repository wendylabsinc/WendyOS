import SwiftUI

/// Synchronous UI events enqueue work; a view's `.task` executes it as child
/// tasks. Leaving the view cancels and joins every action still in progress.
@MainActor
final class AsyncActions {
    typealias Action = @MainActor @Sendable () async -> Void
    private struct Work: Sendable { let operation: Action }
    private var pending: [Work] = []
    private var generation = 0
    private var continuation: AsyncStream<Work>.Continuation?
    private var finished: AsyncCallback<Void>?

    func submit(_ action: @escaping Action) {
        let work = Work(operation: action)
        if let continuation, case .enqueued = continuation.yield(work) { return }
        pending.append(work)
    }

    func run() async {
        // SwiftUI may start a replacement before the cancelled scope has
        // finished its C cleanup. The old scope must not clear the new stream.
        continuation?.finish()
        generation += 1
        let current = generation
        let stream = AsyncStream<Work>.makeStream()
        let finished = AsyncCallback<Void>()
        self.finished = finished
        continuation = stream.continuation
        for action in pending { stream.continuation.yield(action) }
        pending.removeAll()
        await Self.consume(stream.stream)
        if generation == current { continuation = nil; self.finished = nil }
        finished.finish(.success(()))
    }

    private nonisolated static func consume(_ stream: AsyncStream<Work>) async {
        await withDiscardingTaskGroup { group in
            var iterator = stream.makeAsyncIterator()
            while let action = await iterator.next(isolation: nil) {
                guard !Task.isCancelled else { break }
                group.addTask {
                    guard !Task.isCancelled else { return }
                    await action.operation()
                }
            }
            if Task.isCancelled { group.cancelAll() }
        }
    }

    /// App shutdown drains cleanup actions before releasing the Go backend.
    func finishAndWait() async {
        if let continuation, let finished {
            continuation.finish()
            _ = try? await finished.value()
        } else {
            let actions = pending
            pending.removeAll()
            for action in actions { await action.operation() }
        }
    }
}
