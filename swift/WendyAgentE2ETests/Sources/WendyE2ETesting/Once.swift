public actor Once {
    public init() {
        // Intentionally left blank.
    }

    public func perform(_ block: () async -> Void) async {
        guard !done else { return }
        done = true
        await block()
    }

    public func perform(_ block: () async throws -> Void) async throws {
        if let error {
            throw OnceError.failedOnFirstRun(originalError: error)
        }

        guard !done else { return }
        done = true

        do {
            try await block()
        } catch {
            self.error = error
            throw error
        }
    }

    // MARK: - Private

    private var done = false
    private var error: Error? = nil
}
