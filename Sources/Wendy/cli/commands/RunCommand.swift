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
import WendyCLI

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

    func run() async throws {
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

        let title = TerminalText(stringLiteral: "Which device do you want to run this app on?")
        let endpoint = try await agentConnectionOptions.read(title: title)
        try await _withAgentGRPCClient(
            endpoint,
            title: title
        ) { [name] client, endpoint in
            // Bind to all interfaces for Docker Desktop compatibility
            try await withTCPProxyServer(
                localHostname: "0.0.0.0",
                localPort: 50053,
                remoteHostname: endpoint.host,
                remotePort: 5000
            ) { proxyAddress in
                let port = proxyAddress?.port ?? 50053
                let builderName = docker.builderName(forPort: port)

                if try await !docker.hasBuildxBuilder(builderName: builderName) {
                    // Create buildx builder with insecure registry support
                    try await Noora().progressStep(
                        message: "Setting up builder",
                        successMessage: "Builder ready",
                        errorMessage: "Failed to create builder",
                        showSpinner: true
                    ) { _ in
                        try await docker.createBuildxBuilder(port: port)
                    }
                }

                // Build and push in a single operation for better performance
                try await Noora().progressStep(
                    message: "Building and uploading container",
                    successMessage: "Container built and uploaded successfully!",
                    errorMessage: "Failed to build and upload container",
                    showSpinner: true
                ) { _ in
                    try await docker.buildxAndPush(name: name, port: port, builder: builderName)
                }
            }

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

            try await startContainerdContainer(
                imageName: name,
                client: client
            )
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
                    if noRestart {
                        $0.restartPolicy = .with {
                            $0.mode = .no
                        }
                    } else if let retries = restartOnFailureRetries {
                        $0.restartPolicy = .with {
                            $0.mode = .onFailure
                            $0.onFailureMaxRetries = Int32(retries)
                        }
                    } else if restartUnlessStoppedFlag {
                        $0.restartPolicy = .with {
                            $0.mode = .unlessStopped
                        }
                    } else {
                        $0.restartPolicy = .with {
                            $0.mode = .default
                        }
                    }
                }
            )
        )
    }

    func startContainerdContainer(
        imageName: String,
        client: GRPCClient<HTTP2ClientTransport.Posix>
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd.start")
        let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
            wrapping: client
        )

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

                    if detach {
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
    }

    func runSwiftApp() async throws {
        let swiftPM = SwiftPM()
        try await swiftPM.addDependency(
            url: "https://github.com/apple/swift-container-plugin",
            from: "1.0.0"
        )
        let package = try await swiftPM.dumpPackage()

        // Get all executable targets
        let executableTargets = package.targets.filter { $0.type == "executable" }

        // Use specified executable or handle multiple executable targets
        let executableTarget: SwiftPM.Package.Target
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
            try await swiftPM.buildAndPushContainer(
                swiftSDK: swiftSDK,
                product: executableTarget,
                device: endpoint.host
            )

            Noora().info("Creating Container")
            try await createContainerdContainer(
                appName: appName,
                client: client
            )

            Noora().info("Starting Container")
            try await startContainerdContainer(imageName: appName, client: client)
        }
    }

    private func withTCPProxyServer<T: Sendable>(
        localHostname: String,
        localPort: Int,
        remoteHostname: String,
        remotePort: Int,
        _ withPort: @escaping @Sendable (NIOCore.SocketAddress?) async throws -> T
    ) async throws -> T {
        let server = try await ServerBootstrap(group: .singletonMultiThreadedEventLoopGroup)
            .serverChannelOption(ChannelOptions.backlog, value: numericCast(256))
            .serverChannelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
            .childChannelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
            .childChannelOption(ChannelOptions.allowRemoteHalfClosure, value: true)
            .bind(
                host: localHostname,
                port: localPort,
                serverBackPressureStrategy: nil
            ) { channel in
                return channel.eventLoop.makeCompletedFuture {
                    try NIOAsyncChannel<ByteBuffer, ByteBuffer>(
                        wrappingChannelSynchronously: channel,
                        configuration: .init()
                    )
                }
            }

        func makeClient() async throws -> NIOAsyncChannel<ByteBuffer, ByteBuffer> {
            try await ClientBootstrap(group: .singletonMultiThreadedEventLoopGroup)
                .channelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
                .connect(host: remoteHostname, port: remotePort) { channel in
                    return channel.eventLoop.makeCompletedFuture {
                        try NIOAsyncChannel<ByteBuffer, ByteBuffer>(
                            wrappingChannelSynchronously: channel,
                            configuration: .init()
                        )
                    }
                }
        }

        let logger = Logger(label: "sh.wendy.cli.run.tcp-proxy-server")

        func handleClient(client: NIOAsyncChannel<ByteBuffer, ByteBuffer>) async throws {
            do {
                try await client.executeThenClose { serverInbound, serverOutbound in
                    try await makeClient().executeThenClose { clientInbound, clientOutbound in
                        try await withThrowingTaskGroup { group in
                            group.addTask {
                                for try await buffer in serverInbound {
                                    try await clientOutbound.write(buffer)
                                }
                            }
                            group.addTask {
                                for try await buffer in clientInbound {
                                    try await serverOutbound.write(buffer)
                                }
                            }
                            try await group.waitForAll()
                        }
                    }
                }
            } catch is CancellationError {
                // Connection was cancelled (normal when buildx completes)
                logger.trace("Client connection cancelled")
            } catch {
                logger.error("Failed to handle client", metadata: ["error": .string("\(error)")])
            }
        }

        return try await server.executeThenClose { clients in
            try await withThrowingTaskGroup { group in
                group.addTask {
                    try await withThrowingDiscardingTaskGroup { group in
                        for try await client in clients {
                            group.addTask {
                                try await handleClient(client: client)
                            }
                        }
                    }
                }

                defer { group.cancelAll() }
                return try await withPort(server.channel.localAddress)
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
}
