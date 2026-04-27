public struct WendyObservation: Sendable {
    public func cancel() async {
        await self.cancelHandler()
    }

    // MARK: - Internal

    init(cancelHandler: @escaping @Sendable () async -> Void) {
        self.cancelHandler = cancelHandler
    }

    // MARK: - Private

    private let cancelHandler: @Sendable () async -> Void
}
