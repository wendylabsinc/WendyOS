import Foundation
import GRPCCore
import NIOCore
import Noora

public func withErrorTracking(
    _ body: @Sendable () async throws -> Void
) async throws {
    do {
        return try await body()
    } catch let error as RPCError where error.code == .unavailable {
        try await deviceUnreachable()
        return
    } catch {
        Noora().error(
            .alert(
                "An unexpected error occurred: \(error.localizedDescription)",
                takeaways: [
                    "Join our Discord for support: \("https://discord.gg/xYeUxq9TXv".underline)"
                ]
            )
        )
        throw error
    }
}

private func deviceUnreachable() async throws {
    let arguments = ProcessInfo.processInfo.arguments
    if let index = arguments.firstIndex(of: "--device"),
        index + 1 < arguments.count
    {
        try await deviceUnreachable(source: .commandLine(value: arguments[index + 1]))
    } else if let device = ProcessInfo.processInfo.environment["WENDY_AGENT"] {
        try await deviceUnreachable(source: .environment(key: "WENDY_AGENT", value: device))
    } else {
        try await deviceUnreachable(source: .selected)
    }
}

enum DeviceSource {
    case commandLine(value: String)
    case environment(key: String, value: String)
    case selected
}

private func deviceUnreachable(source: DeviceSource) async throws {
    // TODO: Ping device to see if it is reachable, or if the agent is offline
    let takeaways: [TerminalText] = [
        "It may be offline or updating itself.",
        "Connect to the device directly over USB",
        "Check the device is powered on and running.",
        "Discover devices with \("wendy discover".underline)",
        "Join our Discord for support: \("https://discord.gg/xYeUxq9TXv".underline)",
    ]

    switch source {
    case .commandLine(let value):
        Noora().error(
            .alert(
                """
                Device is unreachable: \(value.underline)
                The hostname was provided in the command line arguments.
                """,
                takeaways: takeaways
            )
        )
    case .environment(let key, let value):
        Noora().error(
            .alert(
                """
                Device is unreachable: \(value.underline)
                The hostname was found in the environment variable \(key.underline).
                """,
                takeaways: takeaways
            )
        )
    case .selected:
        Noora().error(
            .alert(
                """
                Selected device is unreachable.
                The hostname was found in the selected device.
                """,
                takeaways: takeaways
            )
        )
    }
}
