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

            // Validate wendy.json early to show warnings before building
            let logger = Logger(label: "sh.wendy.cli.run")
            _ = try await AppBuildHelpers.readAppConfigData(logger: logger)

            let currentPath = FileManager.default.currentDirectoryPath
            let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")
            let directory = try FileManager.default.contentsOfDirectory(atPath: currentPath)

            for item in directory where isDockerfile(item) {
                try await runDockerfileApp()
                return
            }

            if isSwiftPackage {
                try await runSwiftApp()
            } else if isPythonProject(directory: directory) {
                // Python project without Dockerfile - offer to generate one
                try await generatePythonDockerfileAndRun()
            } else {
                Noora().error(
                    "Directory is not a Swift Package, nor can it be built as a docker container"
                )
            }
        }
    }

    /// Checks if a filename is a valid Dockerfile name
    /// Valid names: Dockerfile, dockerfile, Dockerfile.dev, Dockerfile.prod, app.Dockerfile, etc.
    private func isDockerfile(_ filename: String) -> Bool {
        let lowercased = filename.lowercased()
        // Exact match: "dockerfile"
        if lowercased == "dockerfile" {
            return true
        }
        // Pattern: "dockerfile.*" (e.g., dockerfile.dev, Dockerfile.prod)
        if lowercased.hasPrefix("dockerfile.") {
            return true
        }
        // Pattern: "*.dockerfile" (e.g., app.dockerfile)
        if lowercased.hasSuffix(".dockerfile") {
            return true
        }
        return false
    }

    /// Checks if the directory contains a Python project
    private func isPythonProject(directory: [String]) -> Bool {
        // Check for requirements.txt
        if FileManager.default.fileExists(atPath: "requirements.txt") {
            return true
        }
        // Check for pyproject.toml
        if FileManager.default.fileExists(atPath: "pyproject.toml") {
            return true
        }
        // Check for any .py files in the root directory
        return directory.contains { $0.hasSuffix(".py") }
    }

    func generatePythonDockerfileAndRun() async throws {
        let generator = PythonDockerfileGenerator()

        cliOutput.info("Detected Python project")

        // Detect or prompt for entry point
        let entryPoint: String
        if let detected = generator.detectEntryPoint() {
            cliOutput.info("Detected entry point: \(detected)")
            entryPoint = detected
        } else if let selected = generator.promptForEntryPoint(autoAccept: shouldAutoAccept) {
            entryPoint = selected
        } else {
            cliOutput.error(
                "No Python files found in the project. Please create a Python file or add a Dockerfile manually."
            )
            return
        }

        // Show what we detected
        let pythonVersion = generator.getPythonVersion()
        let framework = generator.detectFramework()
        let systemDeps = generator.detectSystemDependencies()

        cliOutput.info("Python version: \(pythonVersion)")
        if framework != .none {
            cliOutput.info("Detected framework: \(framework.displayName)")
        }
        if !systemDeps.isEmpty {
            cliOutput.info("System dependencies: \(systemDeps.joined(separator: ", "))")
        }
        if generator.usesPyTorch() {
            cliOutput.info("PyTorch detected - using Jetson-optimized Dockerfile")
        }

        // Confirm generation
        if !shouldAutoAccept {
            guard
                Noora().yesOrNoChoicePrompt(
                    question: "Generate Dockerfile and continue?"
                )
            else {
                return
            }
        }

        // Generate and write Dockerfile
        try generator.writeDockerfile(entryPoint: entryPoint)
        cliOutput.success("Generated Dockerfile")

        // Now run as a Dockerfile app
        try await runDockerfileApp()
    }

    func runDockerfileApp() async throws {
        try await AppBuildHelpers.checkDockerIsRunning(shouldAutoAccept: shouldAutoAccept)

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

            // Create buildx builder with insecure registry support
            try await AppBuildHelpers.executePhase(
                phase: "builder_setup",
                runtime: "dockerfile",
                commandName: "wendy run",
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
            try await AppBuildHelpers.executePhase(
                phase: "build_upload",
                runtime: "dockerfile",
                commandName: "wendy run",
                additionalProperties: buildPhaseProperties
            ) {
                try await docker.buildxAndPush(
                    name: name,
                    registryHostname: endpoint.host,
                    registryPort: 5000
                )
                cliOutput.success("Container built and uploaded successfully!")
            }

            try await AppBuildHelpers.executePhase(
                phase: "prepare_container",
                runtime: "dockerfile",
                commandName: "wendy run"
            ) {
                try await cliOutput.withProgressBar(
                    message: "Unpacking image on device"
                ) { updateProgress in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: name,
                        client: client,
                        restartPolicy: buildRestartPolicy(),
                        progress: updateProgress
                    )
                }
                cliOutput.success("App \(name) on \(endpoint.host) ready to start")
            }

            try await AppBuildHelpers.executePhase(
                phase: "start_container",
                runtime: "dockerfile",
                commandName: "wendy run"
            ) {
                try await startContainerdContainer(
                    imageName: name,
                    client: client,
                    hostname: endpoint.host
                )
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
                            Noora().success(
                                "Started app \(imageName) on \(hostname) with debug port 4242"
                            )
                        } else {
                            Noora().success("Started app \(imageName) on \(hostname)")
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

    func runSwiftApp() async throws {
        try await AppBuildHelpers.checkSwiftRequirements(
            swiftVersion: swiftVersion,
            swiftSDK: swiftSDK,
            sdkDownloadURL: sdkDownloadURL,
            sdkChecksum: sdkChecksum,
            shouldAutoAccept: shouldAutoAccept
        )

        let swiftPM = SwiftPM()
        let projectPath = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let cache = PackageCache(projectPath: projectPath)

        // Try to use cached data for executables and plugin status
        let allExecutables: [SwiftPM.Executable]
        let packageIdentity: String
        var hasContainerPlugin: Bool

        if let cached = cache.getValidCache() {
            // Cache hit - use cached data
            allExecutables = cached.executables
            packageIdentity = cached.packageIdentity
            hasContainerPlugin = cached.hasContainerPlugin
        } else {
            // Cache miss - fetch fresh data
            let package = try await cliOutput.withProgress(
                message: "Analyzing package structure",
                successMessage: "Package structure analyzed",
                errorMessage: "Failed to analyze package"
            ) {
                try await swiftPM.showDependencies()
            }

            let executables = try await cliOutput.withProgress(
                message: "Finding executables",
                successMessage: "Found executables",
                errorMessage: "Failed to find executables"
            ) {
                try await swiftPM.showExecutables()
            }

            allExecutables = executables
            packageIdentity = package.identity
            hasContainerPlugin = package.dependencies.contains {
                $0.url.hasSuffix("swift-container-plugin")
                    || $0.url.hasSuffix("swift-container-plugin.git")
            }

            // Write to cache
            if let hash = try? cache.computePackageSwiftHash() {
                try? cache.write(
                    PackageCache.CachedPackageInfo(
                        packageSwiftHash: hash,
                        packageIdentity: packageIdentity,
                        executables: allExecutables,
                        hasContainerPlugin: hasContainerPlugin
                    )
                )
            }
        }

        if !hasContainerPlugin {
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

            // Invalidate cache since Package.swift was modified
            cache.invalidate()
            hasContainerPlugin = true
        }

        let executableTargets = allExecutables.filter {
            $0.package == packageIdentity || $0.package == nil
        }

        // Use specified executable or handle multiple executable targets
        let executableTarget: SwiftPM.Executable
        if let executableName = executable {
            guard let target = executableTargets.first(where: { $0.name == executableName }) else {
                throw AppBuildHelpers.Error.invalidExecutableTarget(executableName)
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
            try await AppBuildHelpers.executePhase(
                phase: "build_swift_app",
                runtime: "swift",
                commandName: "wendy run"
            ) {
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

                try await cliOutput.withStreamingOutputBox(
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

            try await AppBuildHelpers.executePhase(
                phase: "create_container",
                runtime: "swift",
                commandName: "wendy run"
            ) {
                try await cliOutput.withProgressBar(
                    message: "Unpacking image on device"
                ) { updateProgress in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: appName,
                        client: client,
                        restartPolicy: buildRestartPolicy(),
                        progress: updateProgress
                    )
                }
                cliOutput.success("App \(appName) on \(endpoint.host) ready to start")
            }

            cliOutput.info("Starting container")
            try await AppBuildHelpers.executePhase(
                phase: "start_container",
                runtime: "swift",
                commandName: "wendy run"
            ) {
                try await startContainerdContainer(
                    imageName: appName,
                    client: client,
                    hostname: endpoint.host
                )
            }
        }
    }

}
