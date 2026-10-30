import Synchronization

/// A single callback result, including cancellation before the waiter is installed.
/// Completion resumes outside the lock and only the first result is delivered.
final class AsyncCallback<Value: Sendable>: Sendable {
    private enum State {
        case pending
        case waiting(CheckedContinuation<Value, Error>)
        case finished(Result<Value, Error>)
    }
    private let state = Mutex<State>(.pending)

    func value() async throws -> Value {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                let result = state.withLock { state -> Result<Value, Error>? in
                    switch state {
                    case .pending: state = .waiting(continuation); return nil
                    case .finished(let result): return result
                    case .waiting: preconditionFailure("An AsyncCallback supports one waiter")
                    }
                }
                if let result { continuation.resume(with: result) }
            }
        } onCancel: {
            self.finish(.failure(CancellationError()))
        }
    }

    func finish(_ result: Result<Value, Error>) {
        let continuation = state.withLock { state -> CheckedContinuation<Value, Error>? in
            switch state {
            case .finished: return nil
            case .pending: state = .finished(result); return nil
            case .waiting(let continuation): state = .finished(result); return continuation
            }
        }
        continuation?.resume(with: result)
    }
}
