import Foundation
import OpenTelemetryGRPC

/// Merge data and timer events into one stream consumed by one RPC writer.
/// The bounded buffer follows TelemetryBroadcaster's existing buffering policy.
enum LogHeartbeatEvent: Sendable {
    case logs(TelemetryBroadcaster.LogsRequest)
    case tick
}

func withLogHeartbeatStream(
    _ logs: AsyncStream<TelemetryBroadcaster.LogsRequest>,
    interval: Duration,
    body: (AsyncStream<LogHeartbeatEvent>) async throws -> Void
) async throws {
    let (events, continuation) = AsyncStream<LogHeartbeatEvent>.makeStream(
        bufferingPolicy: .bufferingNewest(100)
    )
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask {
            for await log in logs { continuation.yield(.logs(log)) }
            continuation.finish()
        }
        group.addTask {
            while !Task.isCancelled {
                try await Task.sleep(for: interval)
                continuation.yield(.tick)
            }
        }
        defer {
            group.cancelAll()
            continuation.finish()
        }
        try await body(events)
    }
}
