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

#if os(macOS)
    import AppKit
#endif

/// Result of bandwidth measurement between CLI and device
struct BandwidthMeasurement: Sendable {
    /// Measured bandwidth in MB/s (megabytes per second)
    let bandwidthMBps: Double
    /// Round-trip latency in milliseconds
    let latencyMs: Double
    /// Recommended compression mode based on measurement
    let recommendedCompression: ImageCompressionMode

    /// Bandwidth threshold in MB/s above which we skip compression
    /// USB is typically 160-190 MB/s, zstd decompresses at ~300-500 MB/s on Jetson
    /// If upload is faster than decompression, uncompressed wins
    static let uncompressedThresholdMBps: Double = 150.0

    /// Bandwidth threshold below which we definitely want compression
    /// WiFi/slow LAN typically < 50 MB/s
    static let compressedThresholdMBps: Double = 50.0
}

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

    @Argument(
        help: "The executable to run. Required when a package has multiple executable targets."
    )
    var executable: String?

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
    }

    func runDockerfileApp() async throws {
        try await checkDockerIsRunning()

        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()
        let dockerContext = await docker.currentContext()

        let title = TerminalText(stringLiteral: "Which device do you want to run this app on?")
        try await withAgentGRPCClientAndEndpoint(
            agentConnectionOptions,
            title: title
        ) { [name, dockerContext] client, endpoint in
            // Build additional properties for analytics
            var buildPhaseProperties: [String: String] = [:]
            if let dockerContext {
                buildPhaseProperties["docker_context"] = dockerContext
            }

            // Measure bandwidth to determine optimal compression
            let compressionMode: ImageCompressionMode = await {
                do {
                    let measurement = try await Noora().progressStep(
                        message: "Measuring connection speed",
                        successMessage: "Connection speed measured",
                        errorMessage: "Failed to measure bandwidth",
                        showSpinner: true
                    ) { _ in
                        try await measureBandwidth(client: client)
                    }
                    let speedStr = String(format: "%.0f", measurement.bandwidthMBps)
                    let modeStr =
                        measurement.recommendedCompression == .uncompressed
                        ? "uncompressed" : "zstd"
                    cliOutput.info("Connection: \(speedStr) MB/s (using \(modeStr) compression)")
                    return measurement.recommendedCompression
                } catch {
                    // If bandwidth measurement fails, fall back to zstd
                    cliOutput.info("Using zstd compression (default)")
                    return ImageCompressionMode.zstd
                }
            }()

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
            let compression = compressionMode  // Capture as let for Sendable
            try await executePhase(
                phase: "build_upload",
                runtime: "dockerfile",
                additionalProperties: buildPhaseProperties
            ) {
                try await cliOutput.withStreamingOutput(
                    title: "Building and uploading container",
                    maxLines: 20
                ) { emit in
                    try await docker.buildxAndPush(
                        name: name,
                        registryHostname: endpoint.host,
                        registryPort: 5000,
                        compression: compression,
                        onOutput: emit
                    )
                }
                cliOutput.success("Container built and uploaded successfully!")
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

    /// Measure bandwidth to the device by sending a test payload
    /// Returns bandwidth in MB/s and recommended compression mode
    func measureBandwidth(
        client: GRPCClient<HTTP2ClientTransport.Posix>
    ) async throws -> BandwidthMeasurement {
        let logger = Logger(label: "sh.wendy.cli.run.bandwidth")
        let agentService = Wendy_Agent_Services_V1_WendyAgentService.Client(
            wrapping: client
        )

        // Use 1MB payload for measurement - large enough to get meaningful bandwidth
        // but small enough to complete quickly
        let payloadSize = 1024 * 1024  // 1 MB
        let payload = Data(repeating: 0xAB, count: payloadSize)

        // Warm up with a small request first (connection establishment, TLS handshake)
        let warmupPayload = Data(repeating: 0xAB, count: 1024)
        _ = try? await agentService.measureBandwidth(
            request: .init(message: .with { $0.payload = warmupPayload })
        )

        // Measure round-trip time for the actual payload
        let startTime = ContinuousClock.now
        _ = try await agentService.measureBandwidth(
            request: .init(message: .with { $0.payload = payload })
        )
        let elapsed = ContinuousClock.now - startTime
        let elapsedSeconds =
            Double(elapsed.components.seconds) + Double(elapsed.components.attoseconds)
            / 1_000_000_000_000_000_000

        // Calculate bandwidth (payload sent + echoed back = 2x payload)
        let totalBytes = Double(payloadSize * 2)
        let bandwidthBps = totalBytes / elapsedSeconds
        let bandwidthMBps = bandwidthBps / (1024 * 1024)
        let latencyMs = elapsedSeconds * 1000

        logger.info(
            "Bandwidth measurement",
            metadata: [
                "bandwidth_mbps": .stringConvertible(String(format: "%.1f", bandwidthMBps)),
                "latency_ms": .stringConvertible(String(format: "%.1f", latencyMs)),
            ]
        )

        // Decide on compression mode based on bandwidth
        let recommendedCompression: ImageCompressionMode
        if bandwidthMBps >= BandwidthMeasurement.uncompressedThresholdMBps {
            // Fast connection (USB, fast LAN) - skip compression
            recommendedCompression = .uncompressed
            logger.info("Fast connection detected, using uncompressed transfer")
        } else if bandwidthMBps <= BandwidthMeasurement.compressedThresholdMBps {
            // Slow connection (WiFi, slow LAN) - use zstd for best compression
            recommendedCompression = .zstd
            logger.info("Slow connection detected, using zstd compression")
        } else {
            // Medium speed - use zstd as it's fast to decompress
            recommendedCompression = .zstd
            logger.info("Medium speed connection, using zstd compression")
        }

        return BandwidthMeasurement(
            bandwidthMBps: bandwidthMBps,
            latencyMs: latencyMs,
            recommendedCompression: recommendedCompression
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
                await stopContainerWithTimeout(
                    imageName: imageName,
                    client: client,
                    timeout: 5.0
                )
            }
            throw error
        }
    }

    func checkDockerIsRunning() async throws {
        let docker = DockerCLI()

        while true {
            do {
                _ = try await docker.getServerVersion()
                return
            } catch {
                // In JSON mode, just fail with an error - cannot prompt to start Docker
                if JSONMode.isEnabled {
                    JSONErrorResponse(
                        error: "docker_not_running",
                        reason: "Docker is not running",
                        suggestion:
                            "Please start Docker Desktop or OrbStack before running this command"
                    ).print()
                    throw ExitCode.failure
                }

                cliOutput.warning("Docker is not running")

                #if os(macOS)
                    // Check if Docker.app, OrbStack.app is installed
                    if let url = NSWorkspace.shared.urlForApplication(
                        withBundleIdentifier: "com.docker.docker"
                    ) {
                        Noora().info("Docker Desktop is installed")

                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open Docker Desktop?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            Noora().info("Opening Docker.app")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            Noora().info(
                                "Failed to open Docker Desktop automatically, please open it manually"
                            )
                        }
                        return
                    } else if let url = NSWorkspace.shared.urlForApplication(
                        withBundleIdentifier: "com.orbstack.orbstack"
                    ) {
                        Noora().info("OrbStack.app is installed")

                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open OrbStack?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(url) {
                            Noora().info("Opening OrbStack")
                            while true {
                                do {
                                    _ = try await docker.getServerVersion()
                                    return
                                } catch {
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                            }
                        } else {
                            Noora().info(
                                "Failed to open OrbStack automatically, please open it manually"
                            )
                        }
                        return
                    } else {
                        cliOutput.warning("Docker.app or OrbStack.app is not installed")
                        guard
                            Noora().yesOrNoChoicePrompt(
                                question: "Do you want to open the installation guide?"
                            )
                        else {
                            return
                        }

                        if NSWorkspace.shared.open(
                            URL(string: "https://docs.docker.com/get-docker/")!
                        ) {
                            Noora().info("Opening Docker documentation")
                        } else {
                            Noora().error("Failed to open Docker documentation")
                        }
                    }
                #endif
            }
        }
    }

    func checkSwiftRequirements() async throws {
        let swiftPM = SwiftPM()

        // Check with spinner
        let (installedSDKs, installedSwiftVersions) = try await cliOutput.withProgress(
            message: "Checking Swift requirements",
            successMessage: "Swift environment ready",
            errorMessage: "Failed to check Swift requirements"
        ) {
            async let sdks = try await swiftPM.listSDKs()
            async let versions = try await swiftPM.listSwiftVersions()
            return try await (sdks, versions)
        }

        if !installedSDKs.contains(swiftSDK) {
            let installSDK: Bool

            if shouldAutoAccept {
                installSDK = true
            } else {
                installSDK = Noora().yesOrNoChoicePrompt(
                    question: "Do you want to install/update the WendyOS Swift SDK?"
                )
            }

            if installSDK {
                try await swiftPM.installSDK(
                    from: sdkDownloadURL,
                    checksum: sdkChecksum
                )
                cliOutput.success("WendyOS SDK ready to use")
            }
        }

        if !installedSwiftVersions.contains(where: { $0.version.name == swiftVersion }) {
            let installSwift: Bool

            if shouldAutoAccept {
                installSwift = true
            } else {
                installSwift = Noora().yesOrNoChoicePrompt(
                    title: "Swift \(swiftVersion) version is not installed yet",
                    question: "Do you want to install Swift \(swiftVersion)?",
                    description: """
                        WendyOS development is tied to a specific Swift toolchain.
                        We update this version from time to time to ensure compatibility with the latest features.
                        """
                )
            }

            if installSwift {
                try await cliOutput.withStreamingOutput(
                    title: "Installing Swift \(swiftVersion)",
                    maxLines: 15
                ) { emit in
                    try await swiftPM.installSDK(
                        from: sdkDownloadURL,
                        checksum: sdkChecksum
                    )
                }
                cliOutput.success("Swift \(swiftVersion) Installed")
            }
        }
    }

    func runSwiftApp() async throws {
        try await checkSwiftRequirements()

        let swiftPM = SwiftPM()
        let package = try await cliOutput.withProgress(
            message: "Analyzing package structure",
            successMessage: "Package structure analyzed",
            errorMessage: "Failed to analyze package"
        ) {
            try await swiftPM.showDependencies()
        }

        if !package.dependencies.contains(where: {
            $0.url.hasSuffix("swift-container-plugin")
                || $0.url.hasSuffix("swift-container-plugin.git")
        }) {
            Noora().info("Container plugin is not installed. Do you want to install it?")

            guard
                shouldAutoAccept
                    || Noora().yesOrNoChoicePrompt(question: "Do you want to install it?")
            else {
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
        let allExecutables = try await cliOutput.withProgress(
            message: "Finding executables",
            successMessage: "Found executables",
            errorMessage: "Failed to find executables"
        ) {
            try await swiftPM.showExecutables()
        }
        let executableTargets = allExecutables.filter {
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
            if JSONMode.isEnabled {
                JSONErrorResponse(
                    error: "no_executable_targets",
                    reason: "No executable targets found in package"
                ).print()
                return
            }
            Noora().error("No executable targets found in package")
            return
        } else if executableTargets.count == 1 {
            executableTarget = executableTargets[0]
        } else if JSONMode.isEnabled {
            // Multiple executable targets and no --executable specified
            jsonModeRequiresArgument(
                argument: "executable",
                description:
                    "Multiple executable targets available: \(executableTargets.map(\.name).joined(separator: ", ")). Provide the target name as an argument."
            )
        } else {
            executableTarget = Noora().singleChoicePrompt(
                title: "Select executable target to run",
                question: "Which executable target do you want to run?",
                options: executableTargets
            )
        }
        // Use the executable target name for image naming to match what swift container plugin uses
        let appName = executableTarget.name.lowercased()
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)

        try await withAgentGRPCClientAndEndpoint(
            agentConnectionOptions,
            title: "Which device do you want to run this app on?"
        ) { client, endpoint in
            try await executePhase(phase: "build_swift_app", runtime: "swift") {
                var resources: [(source: String, destination: String)] = []
                var entrypoint: String?
                let debugArguments = [
                    "gdbserver",
                    "0.0.0.0:4242",
                    "/\(url.lastPathComponent)",
                ]
                var arguments: [String] = []

                if debug {
                    let ds2BinaryName = "ds2-124963fd-static-linux-arm64"
                    // Include the ds2 executable in the container image.
                    if let url = Bundle.module.url(
                        forResource: ds2BinaryName,
                        withExtension: nil
                    ) {
                        resources.append((source: url.path(), destination: "/bin/ds2"))
                        entrypoint = "/bin/ds2"
                        arguments = debugArguments
                    } else {
                        let url = URL(fileURLWithPath: CommandLine.arguments[0])
                            .deletingLastPathComponent()
                            .appending(path: "wendy-agent_wendy.bundle")
                            .appending(path: "Contents")
                            .appending(path: "Resources")
                            .appending(path: "Resources")
                            .appending(component: ds2BinaryName)

                        if FileManager.default.fileExists(atPath: url.path()) {
                            resources.append((source: url.path(), destination: "/bin/ds2"))
                            entrypoint = "/bin/ds2"
                            arguments = debugArguments
                        } else {
                            cliOutput.warning(
                                "ds2 binary not found. Debugging will not be available."
                            )
                        }
                    }
                }

                // Copy to let for Sendable closure capture
                let finalEntrypoint = entrypoint
                let finalArguments = arguments
                let finalResources = resources

                try await cliOutput.withStreamingOutput(
                    title: "Building Swift app",
                    maxLines: 20
                ) { emit in
                    try await swiftPM.buildAndPushContainer(
                        swiftSDK: swiftSDK,
                        product: executableTarget,
                        device: endpoint.host,
                        entrypoint: finalEntrypoint,
                        arguments: finalArguments,
                        resources: finalResources,
                        onOutput: emit
                    )
                }
                cliOutput.success("Swift app built successfully!")
            }

            try await executePhase(phase: "create_container", runtime: "swift") {
                try await cliOutput.withProgress(
                    message: "Creating container",
                    successMessage: "Container created",
                    errorMessage: "Failed to create container"
                ) {
                    try await createContainerdContainer(
                        appName: appName,
                        client: client
                    )
                }
            }

            cliOutput.info("Starting container")
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
        guard let analytics = AnalyticsService.current else { return }
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
