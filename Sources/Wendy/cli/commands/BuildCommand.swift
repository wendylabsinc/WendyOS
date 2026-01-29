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
        help: "The executable to build. Required when a package has multiple executable targets."
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

    func run() async throws {
        try await withErrorTracking {
            let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")
            let directory = try FileManager.default.contentsOfDirectory(
                atPath: FileManager.default.currentDirectoryPath
            )

            for item in directory where item.lowercased().contains("dockerfile") {
                try await buildDockerfileApp()
                return
            }

            if isSwiftPackage {
                try await buildSwiftApp()
            } else {
                Noora().error(
                    "Directory is not a Swift Package, nor can it be built as a docker container"
                )
            }
        }
    }

    func buildDockerfileApp() async throws {
        try await AppBuildHelpers.checkDockerIsRunning(shouldAutoAccept: shouldAutoAccept)

        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()
        let dockerContext = await docker.currentContext()

        let title = TerminalText(stringLiteral: "Which device do you want to build this app for?")
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
                commandName: "wendy build",
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
                commandName: "wendy build",
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
                        onOutput: emit
                    )
                }
                cliOutput.success("Container built and uploaded successfully!")
            }

            try await AppBuildHelpers.executePhase(
                phase: "prepare_container",
                runtime: "dockerfile",
                commandName: "wendy build"
            ) {
                try await Noora().progressStep(
                    message: "Preparing app",
                    successMessage: "App ready",
                    errorMessage: "Failed to prepare app",
                    showSpinner: true
                ) { _ in
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: name,
                        client: client,
                        restartPolicy: .with { $0.mode = .no }
                    )
                }
            }

            cliOutput.success("Build complete! Run 'wendy run' to start the app.")
        }
    }

    func buildSwiftApp() async throws {
        try await AppBuildHelpers.checkSwiftRequirements(
            swiftVersion: swiftVersion,
            swiftSDK: swiftSDK,
            sdkDownloadURL: sdkDownloadURL,
            sdkChecksum: sdkChecksum,
            shouldAutoAccept: shouldAutoAccept
        )

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
                    "Container plugin is required to build Swift packages. Please install it manually."
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
                title: "Select executable target to build",
                question: "Which executable target do you want to build?",
                options: executableTargets
            )
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
                runtime: "swift",
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

            try await AppBuildHelpers.executePhase(
                phase: "create_container",
                runtime: "swift",
                commandName: "wendy build"
            ) {
                try await cliOutput.withProgress(
                    message: "Creating container",
                    successMessage: "Container created",
                    errorMessage: "Failed to create container"
                ) {
                    try await AppBuildHelpers.createContainerdContainer(
                        appName: appName,
                        client: client,
                        restartPolicy: .with { $0.mode = .no }
                    )
                }
            }

            cliOutput.success("Build complete! Run 'wendy run' to start the app.")
        }
    }
}
