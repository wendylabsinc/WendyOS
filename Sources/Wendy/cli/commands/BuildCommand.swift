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

    @Argument(
        help: "The product to build. Required when a package has multiple products."
    )
    var product: String?

    @Argument(
        help: "The executable to build. Required when a package has multiple executable targets."
    )
    var executable: String?

    @Argument(parsing: .captureForPassthrough)
    var passthroughArgs: [String] = []

    @OptionGroup
    var target: TargetOptions

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
    var swiftSDK: String { "\(swiftVersion)-RELEASE_wendyos_aarch64" }
    var sdkDownloadURL: String {
        "https://github.com/wendylabsinc/wendy-swift-tools/releases/download/0.4.0/\(swiftVersion)-RELEASE_wendyos_aarch64.artifactbundle.zip"
    }
    var sdkChecksum: String {
        "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e"
    }

    struct BuiltAppContext: Sendable {
        let app: BuiltApp
        let endpoint: TargetOptions.Endpoint

        enum BuiltApp: Sendable {
            case agent(GRPCClient<GRPCTransport>, appName: String)
            case provider(ProviderBuiltApp)
        }
    }

    func run() async throws {
        try await withErrorTracking {
            try await withContainer(
                restartPolicy: .with { $0.mode = .no }
            ) { _ in
                cliOutput.success("Build complete! Run 'wendy run' to start the app.")
            }
        }
    }

    func withContainer(
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltAppContext
            ) async throws -> Void
    ) async throws {
        let currentPath = FileManager.default.currentDirectoryPath
        let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")
        let directory = try FileManager.default.contentsOfDirectory(atPath: currentPath)

        for item in directory where isDockerfile(item) {
            try await withBuiltDockerfileApp(
                restartPolicy: restartPolicy,
                userArgs: userArgs,
                perform: perform
            )
            return
        }

        if isSwiftPackage {
            try await withBuiltSwiftApp(
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
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltAppContext
            ) async throws -> Void
    ) async throws {
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()
        let dockerContext = await docker.currentContext()

        let target = try await target.read(
            title: "Which device do you want to build this app for?",
            includeLocalProviders: true
        )
        switch target {
        case .external(let device):
            guard let provider = DeviceProviderRegistry.provider(forKey: device.providerKey) else {
                throw CLIError.unsupportedPlatform(
                    reason: "No provider found for '\(device.providerKey)'"
                )
            }

            guard await provider.canBuild(projectPath: url) else {
                throw CLIError.unsupportedPlatform(
                    reason: "Provider '\(device.providerKey)' cannot build Dockerfile projects"
                )
            }

            try await provider.checkRequirements(shouldAutoAccept: shouldAutoAccept)

            let builtApp = try await provider.build(
                for: device,
                projectPath: url,
                executable: name,
                debug: debug
            )

            try await perform(
                BuiltAppContext(
                    app: .provider(builtApp),
                    endpoint: .init(remote: .external(device))
                )
            )
        case .bluetooth:
            throw CLIError.unsupportedPlatform(
                reason: "Bluetooth connections not supported for uploading apps yet"
            )
        case .lan(let host, let port, defaultDevice: _):
            try await AppBuildHelpers.checkDockerIsRunning(shouldAutoAccept: shouldAutoAccept)
            try await withAgentGRPCClient(
                host: host,
                port: port,
                title: "Which device do you want to build this app for?"
            ) { [name, dockerContext] client in
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
                            registryHostname: host,
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
                        registryHostname: host,
                        registryPort: 5000
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
                            progress: updateProgress
                        )
                    }
                    cliOutput.success("App ready")
                }

                try await perform(
                    BuiltAppContext(
                        app: .agent(client, appName: name),
                        endpoint: TargetOptions.Endpoint(host: host, port: port)
                    )
                )
            }
        }
    }

    func withBuiltSwiftApp(
        restartPolicy: RestartPolicy,
        userArgs: [String] = [],
        perform:
            @Sendable @escaping (
                BuiltAppContext
            ) async throws -> Void
    ) async throws {
        // TODO: Swiftly is super slow on airplane mode
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
        let allProducts: [Serialization.Product]
        let packageIdentity: String
        var containerPluginVersion: String? = nil

        if let cached = cache.getValidCache() {
            // Cache hit - use cached data
            allProducts = cached.products
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

            let dump = try await cliOutput.withProgress(
                message: "Finding products",
                successMessage: "Found products",
                errorMessage: "Failed to find products"
            ) {
                try await swiftPM.describe()
            }

            allProducts = dump.products
            packageIdentity = package.identity
            let containerPlugin = package.findDependency(urlSuffix: "swift-container-plugin")

            // Write to cache
            if let hash = try? cache.computePackageSwiftHash() {
                try? cache.write(
                    PackageCache.CachedPackageInfo(
                        packageSwiftHash: hash,
                        packageIdentity: packageIdentity,
                        products: allProducts,
                        hasContainerPlugin: hasContainerPlugin
                    )
                )
            }
        }

        func checkContainerPlugin() async throws {
            if !hasContainerPlugin {
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
                    url: "https://github.com/apple/swift-container-plugin.git",
                    from: "1.3.0"
                )
            }
        }

        // Use specified executable or handle multiple executable targets
        let target: Serialization.Product
        if let executableName = executable {
            guard let product = allProducts.first(where: { product in
                if product.name == executableName, case .executable = product.type {
                    return true
                }
                return false
            }) else {
                throw AppBuildHelpers.Error.invalidExecutableTarget(executableName)
            }
            target = product
        } else if let productName = self.product {
            guard let product = allProducts.first(where: { product in
                if product.name == productName {
                    return true
                }
                return false
            }) else {
                throw AppBuildHelpers.Error.invalidProduct(productName)
            }
            target = product
        } else if allProducts.isEmpty {
            if JSONMode.isEnabled {
                JSONErrorResponse(
                    error: "no_product_targets",
                    reason: "No product targets found in package"
                ).print()
                return
            }
            cliOutput.error("No product targets found in package")
            return
        } else if allProducts.count == 1 {
            target = allProducts[0]
        } else if JSONMode.isEnabled {
            // Multiple executable targets and no --executable specified
            jsonModeRequiresArgument(
                argument: "product",
                description:
                    "Multiple product available: \(allProducts.map(\.name).joined(separator: ", ")). Provide the target name as an argument."
            )
        } else {
            let selectedName = try await cliOutput.singleChoicePrompt(
                title: "Select product to build",
                question: "Which product do you want to build?",
                options: allProducts.map(\.name)
            )
            target = allProducts.first(where: { $0.name == selectedName })!
        }

        // Use the executable target name for image naming to match what swift container plugin uses
        let appName = target.name.lowercased()
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let endpoint = try await self.target.read(
            title: "Which device do you want to build this app for?",
            includeLocalProviders: true
        )

        switch endpoint {
        case .bluetooth:
            throw CLIError.unsupportedPlatform(
                reason: "Bluetooth connections not supported for uploading apps yet"
            )
        case .external(let device):
            guard let provider = DeviceProviderRegistry.provider(forKey: device.providerKey) else {
                throw CLIError.unsupportedPlatform(
                    reason: "No provider found for '\(device.providerKey)'"
                )
            }

            guard await provider.canBuild(projectPath: url) else {
                throw CLIError.unsupportedPlatform(
                    reason: "Provider '\(device.providerKey)' cannot build Swift packages"
                )
            }

            try await provider.checkRequirements(shouldAutoAccept: shouldAutoAccept)

            let builtApp = try await provider.build(
                for: device,
                projectPath: URL(fileURLWithPath: FileManager.default.currentDirectoryPath),
                executable: target.name,
                debug: debug
            )

            try await perform(
                BuiltAppContext(
                    app: .provider(builtApp),
                    endpoint: .init(remote: .external(device))
                )
            )
        case .lan(let host, let port, defaultDevice: _):
            try await checkContainerPlugin()
            try await withAgentGRPCClient(
                host: host,
                port: port,
                title: "Which device do you want to build this app for?"
            ) { client in
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
                            product: target,
                            device: host,
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
                    commandName: "wendy build"
                ) {
                    try await cliOutput.withLabeledProgressBar(
                        message: "Unpacking image on device"
                    ) { updateProgress in
                        try await AppBuildHelpers.createContainerdContainer(
                            appName: appName,
                            client: client,
                            restartPolicy: restartPolicy,
                            progress: updateProgress
                        )
                    }
                    cliOutput.success("Container created")
                }

                try await perform(
                    BuiltAppContext(
                        app: .agent(client, appName: appName),
                        endpoint: TargetOptions.Endpoint(host: host, port: port)
                    )
                )
            }
        }
    }
}
