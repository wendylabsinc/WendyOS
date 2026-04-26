import Foundation

internal struct WendyObservationRegistry<Value: Sendable> {
    internal typealias ObservationHandler = @isolated(any) @Sendable (Value) -> Void
    internal typealias ObservationID = UUID

    internal struct Delivery: Sendable {
        let handler: ObservationHandler
        let value: Value
    }

    init(areEquivalent: @escaping @Sendable (Value, Value) -> Bool) {
        self.areEquivalent = areEquivalent
    }

    mutating func register(
        _ handler: @escaping ObservationHandler,
        initialValue: Value
    ) -> ObservationID {
        let observationID = ObservationID()
        self.observations[observationID] = .init(handler: handler)
        _ = self.enqueue(initialValue, for: observationID)
        return observationID
    }

    mutating func enqueue(_ value: Value) -> [ObservationID] {
        var observationIDs: [ObservationID] = []
        for observationID in self.observations.keys {
            if self.enqueue(value, for: observationID) {
                observationIDs.append(observationID)
            }
        }
        return observationIDs
    }

    mutating func beginDelivery(for observationID: ObservationID) -> Delivery? {
        guard var observationState = self.observations[observationID] else { return nil }
        guard !observationState.pendingValues.isEmpty else {
            observationState.isDelivering = false
            self.observations[observationID] = observationState
            return nil
        }

        let value = observationState.pendingValues.removeFirst()
        observationState.inFlightValue = value
        self.observations[observationID] = observationState
        return .init(handler: observationState.handler, value: value)
    }

    mutating func finishDelivery(for observationID: ObservationID, delivered value: Value) -> Bool {
        guard var observationState = self.observations[observationID] else { return false }

        observationState.lastDeliveredValue = value
        observationState.inFlightValue = nil

        let shouldContinue = !observationState.pendingValues.isEmpty
        observationState.isDelivering = shouldContinue
        self.observations[observationID] = observationState
        return shouldContinue
    }

    mutating func removeObservation(_ observationID: ObservationID) {
        self.observations.removeValue(forKey: observationID)
    }

    // MARK: - Private

    private struct ObservationState {
        let handler: ObservationHandler
        var lastDeliveredValue: Value?
        var inFlightValue: Value?
        var pendingValues: [Value] = []
        var isDelivering = false
    }

    private let areEquivalent: @Sendable (Value, Value) -> Bool
    private var observations: [ObservationID: ObservationState] = [:]

    @discardableResult
    private mutating func enqueue(_ value: Value, for observationID: ObservationID) -> Bool {
        guard var observationState = self.observations[observationID] else { return false }

        if let lastQueuedValue = observationState.pendingValues.last {
            guard !self.areEquivalent(lastQueuedValue, value) else { return false }
        } else if let inFlightValue = observationState.inFlightValue {
            guard !self.areEquivalent(inFlightValue, value) else { return false }
        } else if let lastDeliveredValue = observationState.lastDeliveredValue {
            guard !self.areEquivalent(lastDeliveredValue, value) else { return false }
        }

        observationState.pendingValues.append(value)

        let shouldScheduleDelivery = !observationState.isDelivering
        observationState.isDelivering = true
        self.observations[observationID] = observationState
        return shouldScheduleDelivery
    }
}
