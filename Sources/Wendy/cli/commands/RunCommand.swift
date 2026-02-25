import Analytics
import ArgumentParser
import CLIOutput
import ContainerRegistry
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIO
import Subprocess
import WendyAgentGRPC

#if os(macOS)
    import AppKit
#endif

struct RunCommand: AsyncParsableCommand, Sendable {
    static let configuration = CommandConfiguration(
        commandName: "run",
        abstract: "Run Wendy projects."
    )

    @Flag(name: .long, help: "Attach a debugger to the container")
    var debug: Bool = false

    @Flag(name: .long, help: "Run the container in the background")
    var detach: Bool = false

    @Flag(name: .long, help: "Deploy mode with automatic restarts (up to 5 retries on failure)")
    var deploy: Bool = false

    @Flag(name: .customShort("y"), help: "Auto-accept prompts (required for --json mode)")
    var autoAccept: Bool = false

    /// Whether prompts should be auto-accepted (either explicit -y or JSON mode)
    var shouldAutoAccept: Bool { autoAccept || JSONMode.isEnabled }

    // Docker restart policy flags (mutually exclusive). Only applies to docker runtime.
    @Flag(name: .customLong("no-restart"), help: "Do not restart the container")
    var noRestart: Bool = false

    @Flag(name: .customLong("restart-unless-stopped"), help: "Restart unless stopped")
    var restartUnlessStoppedFlag: Bool = false

    @Option(
        name: .customLong("restart-on-failure"),
        help: "Restart on failure up to N times"
    )
    var restartOnFailureRetries: Int?

    @Option(
        name: .shortAndLong,
        help: "The executable to run. Required when a package has multiple executable targets."
    )
    var executable: String?

    @Argument(parsing: .captureForPassthrough)
    var passthroughArgs: [String] = []

    @OptionGroup
    var agentConnectionOptions: AgentConnectionOptions

    var swiftVersion: String { "6.2.3" }
    var swiftSDK: String { "\(swiftVersion)-RELEASE_wendyos_aarch64" }
    var sdkDownloadURL: String {
        "https://github.com/wendylabsinc/wendy-swift-tools/releases/download/0.4.0/\(swiftVersion)-RELEASE_wendyos_aarch64.artifactbundle.zip"
    }
    var sdkChecksum: String {
        "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e"
    }

    // Deploy mode should always run detached
    var isDetached: Bool { detach || deploy }

    /// CLI args after `--`, with the separator itself filtered out.
    var userPassthroughArgs: [String] {
        passthroughArgs.filter { $0 != "--" }
    }

    /// Validate that flags are not conflicting
    func validate() throws {
        // Count how many restart policy flags are set
        var restartPolicyFlags: [String] = []

        if deploy {
            restartPolicyFlags.append("--deploy")
        }
        if noRestart {
            restartPolicyFlags.append("--no-restart")
        }
        if restartUnlessStoppedFlag {
            restartPolicyFlags.append("--restart-unless-stopped")
        }
        if restartOnFailureRetries != nil {
            restartPolicyFlags.append("--restart-on-failure")
        }

        // If more than one restart policy flag is set, show error
        if restartPolicyFlags.count > 1 {
            throw ValidationError(
                """
                Conflicting restart policy flags detected: \(restartPolicyFlags.joined(separator: ", "))

                Please use only one of:
                  --deploy                    (deploy mode with 5 retries on failure)
                  --no-restart                (never restart)
                  --restart-unless-stopped    (restart unless explicitly stopped)
                  --restart-on-failure N      (restart N times on failure)

                If no flag is provided, development mode is used (no restarts).
                """
            )
        }
    }

    /// Build the restart policy based on the command flags
    /// This determines how containers behave when they exit
    func buildRestartPolicy() -> RestartPolicy {
        if noRestart {
            // Explicit no restart
            return .with { $0.mode = .no }
        } else if let retries = restartOnFailureRetries {
            // Custom retry count on failure
            return .with {
                $0.mode = .onFailure
                $0.onFailureMaxRetries = Int32(retries)
            }
        } else if restartUnlessStoppedFlag {
            // Restart unless explicitly stopped
            return .with { $0.mode = .unlessStopped }
        } else if deploy {
            // Deploy mode: retry up to 5 times on failure
            return .with {
                $0.mode = .onFailure
                $0.onFailureMaxRetries = 5
            }
        } else {
            // Default for development: no restarts
            return .with { $0.mode = .no }
        }
    }

    func run() async throws {
        try await withErrorTracking {
            // Validate flags before proceeding
            try validate()

            try await BuildCommand(
                debug: debug,
                autoAccept: autoAccept,
                executable: executable,
                agentConnectionOptions: agentConnectionOptions
            ).withContainer(
                restartPolicy: buildRestartPolicy(),
                userArgs: userPassthroughArgs
            ) { appName, client, endpoint in
                cliOutput.info("Starting container on \(endpoint.host)")
                try await AppBuildHelpers.executePhase(
                    phase: "start_container",
                    commandName: "wendy run"
                ) {
                    try await startContainerdContainer(
                        imageName: appName.name,
                        client: client,
                        hostname: endpoint.host
                    )
                }
            }
        }
    }

    /// Gracefully stop a container with timeout
    private func stopContainerWithTimeout(
        imageName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        timeout: TimeInterval = 5.0
    ) async {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.stop")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        do {
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask {
                    _ = try await agentContainers.stopContainer(
                        request: .init(
                            message: .with {
                                $0.appName = imageName
                            }
                        )
                    )
                }

                group.addTask {
                    try await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
                    throw CancellationError()
                }

                // Wait for first task to complete (either stop succeeds or timeout)
                try await group.next()
                group.cancelAll()
            }
            logger.info("Container stopped successfully")
        } catch is CancellationError {
            logger.warning(
                "Stop container operation timed out after \(timeout)s",
                metadata: ["container": "\(imageName)"]
            )
        } catch {
            logger.error(
                "Failed to stop container",
                metadata: ["container": "\(imageName)", "error": "\(error)"]
            )
        }
    }

    func startContainerdContainer(
        imageName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        hostname: String
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.start")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        do {
            _ = try await agentContainers.startContainer(
                request: .init(
                    message: .with {
                        $0.appName = imageName
                    }
                )
            ) { response in
                for try await message in response.messages {
                    switch message.responseType {
                    case .started:
                        if debug {
                            cliOutput.success(
                                "Started app \(imageName) on \(hostname) with debug port 4242"
                            )
                        } else {
                            cliOutput.success(
                                "Started app \(imageName) on \(hostname)"
                            )
                        }

                        if isDetached {
                            return
                        }
                    case .stdoutOutput(let stdoutOutput):
                        stdoutOutput.data.withUnsafeBytes { data in
                            #if os(Windows)
                                _ = _write(STDOUT_FILENO, data.baseAddress!, UInt32(data.count))
                            #else
                                _ = write(STDOUT_FILENO, data.baseAddress!, data.count)
                            #endif
                        }
                    case .stderrOutput(let stderrOutput):
                        stderrOutput.data.withUnsafeBytes { data in
                            #if os(Windows)
                                _ = _write(STDERR_FILENO, data.baseAddress!, UInt32(data.count))
                            #else
                                _ = write(STDERR_FILENO, data.baseAddress!, data.count)
                            #endif
                        }
                    default:
                        logger.warning("Unknown message received from agent")
                    }
                }
            }
        } catch {
            // Handle any error (cancellation, network issues, etc.): stop the container when in development mode
            if !isDetached {
                let isCancellation = error is CancellationError
                logger.info(
                    "Container execution \(isCancellation ? "cancelled" : "failed"), stopping container",
                    metadata: ["container": "\(imageName)", "error": "\(error)"]
                )
                await stopContainerWithTimeout(
                    imageName: imageName,
                    client: client,
                    timeout: 5.0
                )
            }
            throw error
        }
    }
}
