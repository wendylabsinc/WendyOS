import Foundation
import NetworkExtension

/// Shared by the desktop and mobile apps. A successful start request is not a
/// connected tunnel; only the system's connection notifications establish that.
@MainActor
enum VPNConnectionLifecycle {
    private enum Event: Sendable { case status(NEVPNStatus), timeout }

    static func start(_ connection: NEVPNConnection, options: [String: NSObject]) async throws {
        let events = AsyncStream<Event>.makeStream()
        let observer = NotificationCenter.default.addObserver(forName: .NEVPNStatusDidChange, object: connection, queue: .main) { notification in
            if let connection = notification.object as? NEVPNConnection {
                events.continuation.yield(.status(connection.status))
            }
        }
        async let _: Void = expire(events.continuation)
        defer {
            NotificationCenter.default.removeObserver(observer)
            events.continuation.finish()
        }
        do {
            try Task.checkCancellation()
            try connection.startVPNTunnel(options: options)
            if connection.status == .connected { return }
            for await event in events.stream {
                try Task.checkCancellation()
                switch event {
                case .status(.connected):
                    if connection.status == .connected { return }
                case .status(.disconnected), .status(.invalid):
                    let error = await disconnectError(connection)
                    throw WendyError(message: error ?? "The VPN disconnected before it could connect.")
                case .timeout:
                    throw WendyError(message: "The VPN did not connect within 60 seconds.")
                default: continue
                }
            }
            throw CancellationError()
        } catch {
            connection.stopVPNTunnel()
            throw error
        }
    }

    private static func expire(_ continuation: AsyncStream<Event>.Continuation) async {
        do {
            try await Task.sleep(for: .seconds(60))
            continuation.yield(.timeout)
        } catch { }
    }

    static func disconnectError(_ connection: NEVPNConnection) async -> String? {
        await withCheckedContinuation { continuation in
            connection.fetchLastDisconnectError { error in
                continuation.resume(returning: error?.localizedDescription)
            }
        }
    }
}
