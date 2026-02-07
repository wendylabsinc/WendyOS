import CLIOutput
import Foundation
import GRPCCore
import NIOCore
import WendyAgentGRPC
import WendyShared

public func withErrorTracking(
    _ body: @Sendable () async throws -> Void
) async throws {
    do {
        return try await body()
    } catch let error as RPCError where error.code == .unavailable {
        if JSONMode.isEnabled {
            JSONErrorResponse(
                error: "device_unavailable",
                reason: "Device is unreachable",
                suggestion: "Check the device is powered on and connected to the network"
            ).print()
            throw error
        }
        try await deviceUnreachable()
        return
    } catch {
        if JSONMode.isEnabled {
            JSONErrorResponse(
                error: "unexpected_error",
                reason: error.localizedDescription
            ).print()
        } else {
            cliOutput.error("An unexpected error occurred: \(error.localizedDescription)")
            cliOutput.info("Join our Discord for support: https://discord.gg/xYeUxq9TXv")
        }
        throw error
    }
}

/// Checks if an error indicates an unimplemented API and prompts the user to update their device.
/// Returns true if the user chose to update and the update was initiated.
func promptDeviceUpdateIfUnimplemented(
    error: any Error,
    endpoint: AgentConnectionOptions.Endpoint
) async -> Bool {
    guard let rpcError = error as? RPCError,
        rpcError.code == .unimplemented
    else {
        return false
    }

    // Don't prompt in JSON mode
    guard !JSONMode.isEnabled else {
        JSONErrorResponse(
            error: "api_not_implemented",
            reason: "The device does not support this feature",
            suggestion: "Update your device with: wendy device update"
        ).print()
        return false
    }

    cliOutput.warning(
        """
        This feature is not available on your device.
        Your device may be running an older version of the Wendy agent.
        """
    )

    let shouldUpdate: Bool
    do {
        shouldUpdate = try await cliOutput.yesOrNoPrompt(
            question: "Would you like to update your device now?",
            defaultAnswer: true
        )
    } catch {
        return false
    }

    guard shouldUpdate else {
        return false
    }

    #if os(Windows)
        cliOutput.warning("Automatic device updates are not supported on Windows.")
        cliOutput.info("Please update your device manually using: wendy device update")
        return false
    #else
        do {
            // Download and apply the update
            let binary = try await downloadLatestRelease(platform: .linuxAarch64).path

            try await withAgentGRPCClient(endpoint, title: "Updating device") { client in
                let agent = Agent(client: client)
                _ = try await cliOutput.withProgressBar(message: "Updating Device") {
                    updateProgress in
                    try await agent.update(fromBinary: binary, onProgress: updateProgress)
                }
            }

            // Wait for the device to restart
            try await waitForDeviceRestart(endpoint: endpoint)

            cliOutput.success("Device updated successfully. Please try your command again.")
            return true
        } catch {
            cliOutput.error("Failed to update device: \(error.localizedDescription)")
            return false
        }
    #endif
}

private func deviceUnreachable() async throws {
    let arguments = ProcessInfo.processInfo.arguments
    if let index = arguments.firstIndex(of: "--device"),
        index + 1 < arguments.count
    {
        try await deviceUnreachable(source: .commandLine(value: arguments[index + 1]))
    } else if let device = ProcessInfo.processInfo.environment["WENDY_AGENT"] {
        try await deviceUnreachable(source: .environment(key: "WENDY_AGENT", value: device))
    } else if let device = getConfig().defaultDevice {
        try await deviceUnreachable(source: .defaultConfig(value: device))
    } else {
        try await deviceUnreachable(source: .selected)
    }
}

enum DeviceSource {
    case commandLine(value: String)
    case environment(key: String, value: String)
    case defaultConfig(value: String)
    case selected
}

private func deviceUnreachable(source: DeviceSource) async throws {
    // TODO: Ping device to see if it is reachable, or if the agent is offline
    let takeaways: [String] = [
        "It may be offline or updating itself.",
        "Connect to the device directly over USB",
        "Check the device is powered on and running.",
        "Discover devices with: wendy discover",
        "Join our Discord for support: https://discord.gg/xYeUxq9TXv",
    ]

    switch source {
    case .commandLine(let value):
        cliOutput.error(
            """
            Device is unreachable: \(value)
            The hostname was provided in the command line arguments.
            """
        )
        for takeaway in takeaways {
            cliOutput.info(takeaway)
        }
    case .environment(let key, let value):
        cliOutput.error(
            """
            Device is unreachable: \(value)
            The hostname was found in the environment variable \(key).
            """
        )
        for takeaway in takeaways {
            cliOutput.info(takeaway)
        }
    case .defaultConfig(let value):
        cliOutput.error(
            """
            Device is unreachable: \(value)
            The hostname was set as a default in the CLI configuration.
            """
        )
        cliOutput.info("Remove the default by running: wendy device unset-default")
        for takeaway in takeaways {
            cliOutput.info(takeaway)
        }
    case .selected:
        cliOutput.error(
            """
            Selected device is unreachable.
            The hostname was found in the selected device.
            """
        )
        for takeaway in takeaways {
            cliOutput.info(takeaway)
        }
    }
}
