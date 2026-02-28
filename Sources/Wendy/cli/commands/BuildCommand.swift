import Analytics
import AppConfig
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

enum DeviceArchitecture: String, Sendable {
    case aarch64
    case x86_64

    var dockerPlatform: String {
        switch self {
        case .aarch64: return "linux/arm64"
        case .x86_64: return "linux/amd64"
        }
    }

    var swiftContainerArchitecture: String {
        switch self {
        case .aarch64: return "arm64"
        case .x86_64: return "amd64"
        }
    }

    var backtraceSearchNames: [String] {
        switch self {
        case .aarch64: return [
            "swift-backtrace-static-linux-arm64",
            "swift-backtrace-linux-arm64",
        ]
        case .x86_64: return [
            "swift-backtrace-static-linux-x86_64",
            "swift-backtrace-linux-x86_64",
        ]
        }
    }

    var ds2BinaryName: String {
        switch self {
        case .aarch64: return "ds2-124963fd-static-linux-arm64"
        case .x86_64: return "ds2-124963fd-static-linux-x86_64"
        }
    }

    init(fromDeviceString arch: String) throws {
        switch arch.lowercased() {
        case "aarch64", "arm64":
            self = .aarch64
        case "x86_64", "amd64":
            self = .x86_64
        default:
            throw BuildError.unsupportedArchitecture(arch)
        }
    }
}

enum BuildError: Error, LocalizedError {
    case unsupportedArchitecture(String)

    var errorDescription: String? {
        switch self {
        case .unsupportedArchitecture(let arch):
            return "Unsupported device architecture: \(arch)"
        }
    }
}

struct BuildCommand: AsyncParsableCommand, Sendable {
    static let configuration = CommandConfiguration(
        commandName: "build",
        abstract: "Build and push Wendy projects without starting them."
    )

    @Flag(name: .long, help: "Include debug resources in the container")
    var debug: Bool = false

    @Flag(name: .customShort("y"), help: "Auto-accept prompts (required for --json mode)")
    var autoAccept: Bool = false

    /// Whether prompts should be auto-accepted (either explicit -y or JSON mode)
    var shouldAutoAccept: Bool { autoAccept || JSONMode.isEnabled }

    @Option(
        name: .shortAndLong,
        help: "The executable to build. Required when a package has multiple executable targets."
    )
    var executable: String?

    @Argument(parsing: .captureForPassthrough)
    var passthroughArgs: [String] = []

    @OptionGroup
    var agentConnectionOptions: AgentConnectionOptions

    init() {}

    init(
        debug: Bool,
        autoAccept: Bool,
        executable: String?,
        passthroughArgs: [String] = [],
        agentConnectionOptions: AgentConnectionOptions
    ) {
        self.debug = debug
        self.autoAccept = autoAccept
        self.executable = executable
        self.passthroughArgs = passthroughArgs
        self.agentConnectionOptions = agentConnectionOptions
    }

    var swiftVersion: String { "6.2.3" }

    func swiftSDK(for architecture: DeviceArchitecture) -> String {
        "\(swiftVersion)-RELEASE_wendyos_\(architecture.rawValue)"
    }

    func sdkDownloadURL(for architecture: DeviceArchitecture) -> String {
        "https://github.com/wendylabsinc/wendy-swift-tools/releases/download/0.4.0/\(swiftVersion)-RELEASE_wendyos_\(architecture.rawValue).artifactbundle.zip"
    }

    func sdkChecksum(for architecture: DeviceArchitecture) -> String {
        switch architecture {
        case .aarch64:
            return "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e"
        case .x86_64:
            return "b5a4d08ad4d4841043727f6671c6aa004da3a2b7f12dc28101d6770c1dc57eb1"
        }
    }

    /// CLI args after `--`, with the separator itself filtered out.
    var userPassthroughArgs: [String] {
        passthroughArgs.filter { $0 != "--" }
    }

    struct BuiltApp: Sendable {
        let name: String
    }

    func run() async throws {
        try await withErrorTracking {
            try await withContainer(
                restartPolicy: .with { $0.mode = .no },
                userArgs: userPassthroughArgs
            ) { _, _, _ in
                cliOutput.success("Build complete! Run 'wendy run' to start the app.")
            }
        }
    }

    func queryDeviceArchitecture() async throws -> DeviceArchitecture {
        try await withAgentGRPCClient(
            agentConnectionOptions,
            title: "Detecting device architecture"
        ) { client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let version = try await agent.getAgentVersion(request: .init(message: .init()))
            return try DeviceArchitecture(fromDeviceString: version.cpuArchitecture)
        }
    }

    func withContainer(
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltApp, GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint
            ) async throws -> Void
    ) async throws {
        let currentPath = FileManager.default.currentDirectoryPath
        let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")
        let directory = try FileManager.default.contentsOfDirectory(atPath: currentPath)

        // Query target device architecture before building
        let architecture = try await queryDeviceArchitecture()

        for item in directory where isDockerfile(item) {
            try await withBuiltDockerfileApp(
                architecture: architecture,
                restartPolicy: restartPolicy,
                userArgs: userArgs,
                perform: perform
            )
            return
        }

        if isSwiftPackage {
            try await withBuiltSwiftApp(
                architecture: architecture,
                restartPolicy: restartPolicy,
                userArgs: userArgs,
                perform: perform
            )
        } else if isPythonProject(directory: directory) {
            // Python project without Dockerfile - offer to generate one
            try await generatePythonDockerfileAndBuild()

            // After attempting generation, verify a Dockerfile now exists
            let updatedDirectory = try FileManager.default.contentsOfDirectory(atPath: currentPath)
            let hasDockerfile = updatedDirectory.contains { isDockerfile($0) }
            guard hasDockerfile else {
                // User may have declined generation or generation failed gracefully
                return
            }
            // Now build as a Dockerfile app
            try await withBuiltDockerfileApp(
                architecture: architecture,
                restartPolicy: restartPolicy,
                userArgs: userArgs,
                perform: perform
            )
        } else {
            cliOutput.error(
                "Directory is not a Swift Package, nor can it be built as a docker container"
            )
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

    func generatePythonDockerfileAndBuild() async throws {
        let generator = PythonDockerfileGenerator()

        cliOutput.info("Detected Python project")

        // Detect or prompt for entry point
        let entryPoint: String
        if let detected = generator.detectEntryPoint() {
            cliOutput.info("Detected entry point: \(detected)")
            entryPoint = detected
        } else if let selected = try await generator.promptForEntryPoint(
            autoAccept: shouldAutoAccept
        ) {
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
                try await cliOutput.yesOrNoPrompt(
                    question: "Generate Dockerfile and continue?",
                    defaultAnswer: true
                )
            else {
                return
            }
        }

        // Generate and write Dockerfile
        try generator.writeDockerfile(entryPoint: entryPoint)
        cliOutput.success("Generated Dockerfile")
    }

    func withBuiltDockerfileApp(
        architecture: DeviceArchitecture,
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltApp, GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint
            ) async throws -> Void
    ) async throws {
        try await AppBuildHelpers.checkDockerIsRunning(shouldAutoAccept: shouldAutoAccept)

        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()
        let dockerContext = await docker.currentContext()

        try await withAgentGRPCClientAndEndpoint(
            agentConnectionOptions,
            title: "Which device do you want to build this app for?"
        ) { [name, dockerContext, userArgs, architecture] client, endpoint in
            // Build additional properties for analytics
            var buildPhaseProperties: [String: String] = [:]
            if let dockerContext {
                buildPhaseProperties["docker_context"] = dockerContext
            }

            // Create buildx builder with insecure registry support
            try await AppBuildHelpers.executePhase(
                phase: "builder_setup",
                commandName: "wendy build",
                additionalProperties: buildPhaseProperties
            ) {
                try await cliOutput.withProgress(
                    message: "Preparing builder",
                    successMessage: "Builder ready",
                    errorMessage: "Failed to create builder"
                ) {
                    try await docker.prepareBuildxBuilder(
                        registryHostname: endpoint.host,
                        registryPort: 5000
                    )
                }
            }

            // Build and push in a single operation for better performance
            try await AppBuildHelpers.executePhase(
                phase: "build_upload",
                commandName: "wendy build",
                additionalProperties: buildPhaseProperties
            ) {
                try await docker.buildxAndPush(
                    name: name,
                    registryHostname: endpoint.host,
                    registryPort: 5000,
                    platform: architecture.dockerPlatform
                )
                cliOutput.success("Container built and uploaded successfully!")
            }

            try await AppBuildHelpers.executePhase(
                phase: "prepare_container",
                commandName: "wendy build"
            ) {
                try await cliOutput.withLabeledProgressBar(
                    message: "Unpacking image on device"
                ) { updateProgress in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: name,
                        client: client,
                        restartPolicy: restartPolicy,
                        userArgs: userArgs,
                        progress: updateProgress
                    )
                }
            }

            try await perform(
                BuiltApp(name: name),
                client,
                endpoint
            )
        }
    }

    func withBuiltSwiftApp(
        architecture: DeviceArchitecture,
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltApp, GRPCClient<GRPCTransport>, AgentConnectionOptions.Endpoint
            ) async throws -> Void
    ) async throws {
        try await AppBuildHelpers.checkSwiftRequirements(
            swiftVersion: swiftVersion,
            swiftSDK: swiftSDK(for: architecture),
            sdkDownloadURL: sdkDownloadURL(for: architecture),
            sdkChecksum: sdkChecksum(for: architecture),
            shouldAutoAccept: shouldAutoAccept
        )

        let swiftPM = SwiftPM()
        let projectPath = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let cache = PackageCache(projectPath: projectPath)

        // Try to use cached data for executables and plugin status
        let allExecutables: [SwiftPM.Executable]
        let packageIdentity: String
        var containerPluginVersion: String? = nil

        if let cached = cache.getValidCache() {
            // Cache hit - use cached data
            allExecutables = cached.executables
            packageIdentity = cached.packageIdentity
            containerPluginVersion = cached.containerPluginVersion
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
            let containerPlugin = package.findDependency(urlSuffix: "swift-container-plugin")

            // Write to cache
            if let hash = try? cache.computePackageSwiftHash() {
                try? cache.write(
                    PackageCache.CachedPackageInfo(
                        packageSwiftHash: hash,
                        packageIdentity: packageIdentity,
                        executables: allExecutables,
                        containerPluginVersion: containerPlugin?.version
                    )
                )
            }
        }

        let containerPluginURL = "https://github.com/apple/swift-container-plugin.git"
        let requiredContainerPluginVersion = "1.3.0"
        let pluginSupportsBacktrace: Bool
        if let containerPluginVersion {
            // Plugin exists, check version
            if !SwiftPM.isVersion(containerPluginVersion, atLeast: requiredContainerPluginVersion) {
                cliOutput.warning(
                    "swift-container-plugin version \(containerPluginVersion) is installed, but version \(requiredContainerPluginVersion) or higher is recommended"
                )
                pluginSupportsBacktrace = false
            } else {
                pluginSupportsBacktrace = true
            }
        } else {
            cliOutput.info(
                "Container plugin is not installed. Do you want to install it?"
            )

            let accepted: Bool
            if shouldAutoAccept {
                accepted = true
            } else {
                accepted = try await cliOutput.yesOrNoPrompt(
                    question: "Do you want to install it?",
                    defaultAnswer: true
                )
            }

            guard accepted else {
                cliOutput.error(
                    "Container plugin is required to build Swift packages. Please install it manually."
                )
                return
            }

            try await swiftPM.addDependency(
                url: containerPluginURL,
                from: requiredContainerPluginVersion
            )
            pluginSupportsBacktrace = true
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
            cliOutput.error("No executable targets found in package")
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
            let selectedName = try await cliOutput.singleChoicePrompt(
                title: "Select executable target to build",
                question: "Which executable target do you want to build?",
                options: executableTargets.map(\.name)
            )
            executableTarget = executableTargets.first(where: { $0.name == selectedName })!
        }

        // Use the executable target name for image naming to match what swift container plugin uses
        let appName = executableTarget.name.lowercased()
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)

        try await withAgentGRPCClientAndEndpoint(
            agentConnectionOptions,
            title: "Which device do you want to build this app for?"
        ) { client, endpoint in
            try await AppBuildHelpers.executePhase(
                phase: "build_swift_app",
                commandName: "wendy build"
            ) {
                var resources: [(source: String, destination: String)] = []
                var entrypoint: String?
                let debugArguments = [
                    "gdbserver",
                    "0.0.0.0:4242",
                    "/\(url.lastPathComponent)",
                ]
                var arguments: [String] = []
                var additionalEnv: [String] = []
                var hasBacktrace = false

                // Add swift-backtrace binaries for crash reporting
                findBacktrace: for binaryName in architecture.backtraceSearchNames
                    where pluginSupportsBacktrace {
                    let destination = "/swift-backtrace"
                    if let backtraceUrl = Bundle.module.url(
                        forResource: binaryName,
                        withExtension: nil
                    ) {
                        hasBacktrace = true
                        resources.append((source: backtraceUrl.path(), destination: destination))
                        break findBacktrace
                    }
                    let backtraceUrl = URL(fileURLWithPath: CommandLine.arguments[0])
                        .deletingLastPathComponent()
                        .appending(path: "wendy-agent_wendy.bundle")
                        .appending(path: "Contents")
                        .appending(path: "Resources")
                        .appending(path: "Resources")
                        .appending(component: binaryName)

                    if FileManager.default.fileExists(atPath: backtraceUrl.path()) {
                        hasBacktrace = true
                        resources.append(
                            (source: backtraceUrl.path(), destination: destination)
                        )
                        break findBacktrace
                    }
                }

                if hasBacktrace {
                    additionalEnv.append(
                        "SWIFT_BACKTRACE=enable=yes,sanitize=yes,threads=all,images=all,interactive=no,swift-backtrace=/swift-backtrace"
                    )
                } else {
                    cliOutput.warning(
                        "swift-backtrace binary not found. Crash backtraces will not be available."
                    )
                }

                if debug {
                    let ds2BinaryName = architecture.ds2BinaryName
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
                ) { [additionalEnv] emit in
                    try await swiftPM.buildAndPushContainer(
                        swiftSDK: swiftSDK(for: architecture),
                        product: executableTarget,
                        device: endpoint.host,
                        architecture: architecture.swiftContainerArchitecture,
                        entrypoint: finalEntrypoint,
                        additionalEnv: additionalEnv,
                        arguments: finalArguments,
                        resources: finalResources,
                        onOutput: emit
                    )
                }
                cliOutput.success("Swift app built successfully!")
            }

            try await AppBuildHelpers.executePhase(
                phase: "create_container",
                commandName: "wendy build"
            ) {
                try await cliOutput.withLabeledProgressBar(
                    message: "Unpacking image on device"
                ) { updateProgress in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: appName,
                        client: client,
                        restartPolicy: restartPolicy,
                        userArgs: userArgs,
                        progress: updateProgress
                    )
                }
            }

            try await perform(
                BuiltApp(name: appName),
                client,
                endpoint
            )
        }
    }
}
