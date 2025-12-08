import Analytics
import AppConfig
import ArgumentParser
import ContainerRegistry
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIO
import NIOFileSystem
import Noora
import Subprocess
import WendyAgentGRPC

struct RunCommand: AsyncParsableCommand, Sendable {
    enum Error: Swift.Error, CustomStringConvertible {
        case failedToUploadLayers(Int)
        case noExecutableTarget
        case invalidExecutableTarget(String)
        case multipleExecutableTargets([String])
        case noManifestFound

        var description: String {
            switch self {
            case .failedToUploadLayers:
                return "Failed to upload"
            case .noExecutableTarget:
                return "No executable target found in package"
            case .invalidExecutableTarget(let name):
                return "No executable target named '\(name)' found in package"
            case .multipleExecutableTargets(let names):
                return
                    "multiple executable targets available, but none specified: \(names.joined(separator: ", "))"
            case .noManifestFound:
                return "No manifest found in Docker image"
            }
        }
    }

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

    @Argument(
        help: "The executable to run. Required when a package has multiple executable targets."
    )
    var executable: String?

    @OptionGroup
    var agentConnectionOptions: AgentConnectionOptions

    var swiftVersion: String { "6.2.1" }
    var swiftSDK: String { "\(swiftVersion)-RELEASE_wendyos_aarch64" }

    // Deploy mode should always run detached
    var isDetached: Bool { detach || deploy }

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
        // Validate flags before proceeding
        try validate()

        let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")
        let directory = try FileManager.default.contentsOfDirectory(
            atPath: FileManager.default.currentDirectoryPath
        )

        for item in directory where item.lowercased().contains("dockerfile") {
            try await runDockerfileApp()
            return
        }

        if isSwiftPackage {
            try await runSwiftApp()
        } else {
            Noora().error(
                "Directory is not a Swift Package, nor can it be built as a docker container"
            )
        }
    }

    func runDockerfileApp() async throws {
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()
        let dockerContext = await docker.currentContext()

        let title = TerminalText(stringLiteral: "Which device do you want to run this app on?")
        let endpoint = try await agentConnectionOptions.read(title: title)
        try await _withAgentGRPCClient(
            endpoint,
            title: title
        ) { [name, dockerContext] client, endpoint in
            // Build additional properties for analytics
            var buildPhaseProperties: [String: String] = [:]
            if let dockerContext {
                buildPhaseProperties["docker_context"] = dockerContext
            }

            // Create buildx builder with insecure registry support
            try await executePhase(
                phase: "builder_setup",
                runtime: "dockerfile",
                additionalProperties: buildPhaseProperties
            ) {
                try await Noora().progressStep(
                    message: "Preparing builder",
                    successMessage: "Builder ready",
                    errorMessage: "Failed to create builder",
                    showSpinner: true
                ) { _ in
                    try await docker.prepareBuildxBuilder(
                        registryHostname: endpoint.host,
                        registryPort: 5000
                    )
                }
            }

            // Build and push in a single operation for better performance
            try await executePhase(
                phase: "build_upload",
                runtime: "dockerfile",
                additionalProperties: buildPhaseProperties
            ) {
                try await Noora().progressStep(
                    message: "Building and uploading container",
                    successMessage: "Container built and uploaded successfully!",
                    errorMessage: "Failed to build and upload container",
                    showSpinner: true
                ) { _ in
                    try await docker.buildxAndPush(
                        name: name,
                        registryHostname: endpoint.host,
                        registryPort: 5000
                    )
                }
            }

            try await executePhase(phase: "prepare_container", runtime: "dockerfile") {
                try await Noora().progressStep(
                    message: "Preparing app",
                    successMessage: "App ready to start",
                    errorMessage: "Failed to prepare app",
                    showSpinner: true
                ) { _ in
                    try await createContainerdContainer(
                        appName: name,
                        client: client
                    )
                }
            }

            try await executePhase(phase: "start_container", runtime: "dockerfile") {
                try await startContainerdContainer(
                    imageName: name,
                    client: client
                )
            }
        }
    }

    func createContainerdContainer(
        appName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.create")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        let appConfigData = try await readAppConfigData(logger: logger)
        _ = try await agentContainers.createContainer(
            request: .init(
                message: .with {
                    // The image is pushed to the device's local registry as just "appName"
                    // The host.docker.internal:port prefix is only for routing during push
                    $0.imageName = "\(appName):latest"
                    $0.appName = appName
                    $0.appConfig = appConfigData
                    $0.restartPolicy = buildRestartPolicy()
                }
            )
        )
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
        client: GRPCClient<HTTP2ClientTransport.Posix>
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.start")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

        do {
            _ = try await agentContainers.startContainer(
                request: ClientRequest<Wendy_Agent_Services_V1_StartContainerRequest>(
                    message: Wendy_Agent_Services_V1_StartContainerRequest.with {
                        $0.appName = imageName
                    }
                )
            ) { response in
                for try await message in response.messages {
                    switch message.responseType {
                    case .started:
                        if debug {
                            Noora().success("Started container with debug port 4242")
                        } else {
                            Noora().success("Started app")
                        }

                        if isDetached {
                            return
                        }
                    case .stdoutOutput(let stdoutOutput):
                        stdoutOutput.data.withUnsafeBytes { data in
                            _ = write(STDOUT_FILENO, data.baseAddress!, data.count)
                        }
                    case .stderrOutput(let stderrOutput):
                        stderrOutput.data.withUnsafeBytes { data in
                            _ = write(STDERR_FILENO, data.baseAddress!, data.count)
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
                // Wait for container to stop before re-throwing
                await stopContainerWithTimeout(
                    imageName: imageName,
                    client: client,
                    timeout: 5.0
                )
            }
            throw error
        }
    }

    func runSwiftApp() async throws {
        let swiftPM = SwiftPM()
        let package = try await swiftPM.showDependencies()

        if !package.dependencies.contains(where: {
            $0.url == "https://github.com/apple/swift-container-plugin"
        }) {
            Noora().info("Container plugin is not installed. Do you want to install it?")

            guard Noora().yesOrNoChoicePrompt(question: "Do you want to install it?") else {
                Noora().error(
                    "Container plugin is required to build and run Swift packages. Please install it manually."
                )
                return
            }

            try await swiftPM.addDependency(
                url: "https://github.com/apple/swift-container-plugin",
                from: "1.0.0"
            )
        }

        // Get all executable targets
        let executableTargets = try await swiftPM.showExecutables().filter {
            $0.package == package.identity || $0.package == nil
        }

        // Use specified executable or handle multiple executable targets
        let executableTarget: SwiftPM.Executable
        if let executableName = executable {
            guard let target = executableTargets.first(where: { $0.name == executableName }) else {
                throw Error.invalidExecutableTarget(executableName)
            }
            executableTarget = target
        } else if executableTargets.isEmpty {
            Noora().error("No executable targets found in package")
            return
        } else {
            executableTarget = Noora().singleChoicePrompt(
                title: "Select executable target to run",
                question: "Which executable target do you want to run?",
                options: executableTargets
            )
        }
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let appName = url.lastPathComponent.lowercased()

        try await withAgentGRPCClientAndEndpoint(
            agentConnectionOptions,
            title: "Which device do you want to run this app on?"
        ) { client, endpoint in
            Noora().info("Building Swift app")
            try await executePhase(phase: "build_swift_app", runtime: "swift") {
                try await swiftPM.buildAndPushContainer(
                    swiftSDK: swiftSDK,
                    product: executableTarget,
                    device: endpoint.host
                )
            }

            Noora().info("Creating Container")
            try await executePhase(phase: "create_container", runtime: "swift") {
                try await createContainerdContainer(
                    appName: appName,
                    client: client
                )
            }

            Noora().info("Starting Container")
            try await executePhase(phase: "start_container", runtime: "swift") {
                try await startContainerdContainer(imageName: appName, client: client)
            }
        }
    }

    private func readAppConfigData(logger: Logger) async throws -> Data {
        do {
            let appConfigData = try Data(contentsOf: URL(fileURLWithPath: "./wendy.json"))
            // Validate data
            _ = try JSONDecoder().decode(AppConfig.self, from: appConfigData)
            return appConfigData
        } catch {
            logger.debug("Failed to decode app config", metadata: ["error": .string("\(error)")])
            Noora().info("No valid wendy.json was found. Using default settings.")
            return Data()
        }
    }

    // MARK: - Phase Analytics

    /// Track a phase failure for analytics
    private func trackPhaseFailure(
        phase: String,
        runtime: String,
        error: Swift.Error,
        additionalProperties: [String: String] = [:]
    ) async {
        guard let analytics = AnalyticsService.shared else { return }
        let sanitizedError = ErrorSanitizer.sanitize(error)
        var properties: [String: String] = [
            "phase": phase,
            "runtime": runtime,
            "command_name": "wendy run",
            "error_type": sanitizedError.type,
            "error_name": sanitizedError.name,
            "error_domain": sanitizedError.domain,
        ]
        for (key, value) in additionalProperties {
            properties[key] = value
        }
        await analytics.trackEvent(
            name: "run_phase_failed",
            properties: properties
        )
    }

    /// Execute a phase with failure tracking
    private func executePhase<T: Sendable>(
        phase: String,
        runtime: String,
        additionalProperties: [String: String] = [:],
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        do {
            return try await operation()
        } catch {
            await trackPhaseFailure(
                phase: phase,
                runtime: runtime,
                error: error,
                additionalProperties: additionalProperties
            )
            throw error
        }
    }
}
