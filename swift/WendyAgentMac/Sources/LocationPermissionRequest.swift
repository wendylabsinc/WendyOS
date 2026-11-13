/// Bounds an authorization wait without confusing request submission with a decision.
/// All completion paths run on the main actor and can safely finish more than once.
@MainActor
final class LocationPermissionRequest {
    private var continuation: CheckedContinuation<Void, Never>?
    private var timeoutTask: Task<Void, Never>?

    var isWaiting: Bool {
        self.continuation != nil
    }

    func wait(
        timeout: Duration,
        onTimeout: @escaping @MainActor () -> Void,
        start: @MainActor () -> Void
    ) async {
        precondition(!self.isWaiting)
        await withCheckedContinuation { continuation in
            self.continuation = continuation
            self.timeoutTask = Task { [weak self] in
                do {
                    try await Task.sleep(for: timeout)
                } catch {
                    return
                }

                guard let self, self.isWaiting else { return }
                onTimeout()
                self.finish()
            }
            start()
        }
    }

    func finish() {
        guard let continuation = self.continuation else { return }
        self.continuation = nil
        self.timeoutTask?.cancel()
        self.timeoutTask = nil
        continuation.resume()
    }
}
