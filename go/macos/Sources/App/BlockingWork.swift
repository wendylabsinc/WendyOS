import Foundation
import Synchronization

/// The Go ABI blocks OS threads. Keep it off Swift's cooperative executor while
/// preserving the caller's lifetime: cancellation skips queued work, but a call
/// already in C must finish before its resources can be released.
enum BlockingWork {
    static func run<Value: Sendable>(
        on queue: DispatchQueue,
        unlessCancelled: Bool = true,
        _ operation: @escaping @Sendable () throws -> Value
    ) async throws -> Value {
        let cancelled = Mutex(false)
        return try await withTaskCancellationHandler {
            if unlessCancelled { try Task.checkCancellation() }
            let value = try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Value, Error>) in
                queue.async {
                    continuation.resume(with: Result {
                        if unlessCancelled, cancelled.withLock({ $0 }) { throw CancellationError() }
                        return try operation()
                    })
                }
            }
            if unlessCancelled { try Task.checkCancellation() }
            return value
        } onCancel: {
            cancelled.withLock { $0 = true }
        }
    }
}
