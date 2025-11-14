import AppConfig
import ArgumentParser
import ContainerBuilder
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

public enum ContainerRuntime: String, ExpressibleByArgument, Sendable {
    case docker
    case containerd
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

    @Option(name: .long, help: "The runtime to use, either `docker` or `containerd`")
    var runtime: ContainerRuntime = .containerd

    @Option(name: .long, help: "The Swift SDK to use.")
    var swiftSDK: String = "6.2.1-RELEASE_wendyos_aarch64"

    @Option(name: .long, help: "The Swift SDK to use.")
    var swiftVersion: String = "+6.2.1"

    @Option(name: .long, help: "The base image to use. Defaults to debian:bookworm-slim.")
    var baseImage: String = "debian:bookworm-slim"

    @Argument(
        help: "The executable to run. Required when a package has multiple executable targets."
    )
    var executable: String?

    @OptionGroup var agentConnectionOptions: AgentConnectionOptions

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.run")
        let isSwiftPackage = FileManager.default.fileExists(atPath: "Package.swift")

        if isSwiftPackage {
            switch runtime {
            case .docker:
                try await runSwiftDockerBased()
            case .containerd:
                try await runSwiftContainerdBased()
            }
        } else {
            let directory = try FileManager.default.contentsOfDirectory(
                atPath: FileManager.default.currentDirectoryPath
            )

            for item in directory where item.lowercased().contains("dockerfile") {
                switch runtime {
                case .docker:
                    try await runDockerBased()
                case .containerd:
                    try await runContainerdBased()
                }
                return
            }

            logger.error(
                "Directory is not a Swift Package, nor can it be built as a docker container"
            )
        }
    }

    /// Compute the restart policy based on CLI flags
    private func computeRestartPolicy() -> RestartPolicy {
        var restartPolicy = RestartPolicy()

        if noRestart {
            restartPolicy.mode = .no
        } else if let retries = restartOnFailureRetries {
            restartPolicy.mode = .onFailure
            restartPolicy.onFailureMaxRetries = Int32(retries)
        } else if restartUnlessStoppedFlag {
            restartPolicy.mode = .unlessStopped
        } else {
            restartPolicy.mode = .default
        }

        return restartPolicy
    }

    func runDockerBased() async throws {
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let appConfigData = try await readAppConfigData(
            logger: Logger(label: "sh.wendy.cli.run.docker.container.docker")
        )

        try await Noora().progressStep(
            message: "Building container",
            successMessage: "Container built successfully!",
            errorMessage: "Failed to build container",
            showSpinner: true
        ) { _ in
            try await buildDockerBased(name: name)
        }

        let output = FileManager.default.temporaryDirectory.appending(component: UUID().uuidString)
            .path()
        try await uploadDockerTar(
            imageName: url.lastPathComponent.lowercased(),
            appConfigData: appConfigData,
            builtContainer: Task {
                try await DockerCLI().save(
                    name: name,
                    output: output
                )
                return output
            }
        )
    }

    func withTCPProxyServer<T: Sendable>(
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

    func runContainerdBased() async throws {
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let name = url.lastPathComponent.lowercased()

        let docker = DockerCLI()

        let title = TerminalText(stringLiteral: "Which device do you want to run this app on?")
        let endpoint = try await agentConnectionOptions.read(title: title)
        try await _withAgentGRPCClient(
            endpoint,
            title: title
        ) { [name] client, endpoint in
            try await withTCPProxyServer(
                localHostname: "localhost",
                localPort: 0,
                remoteHostname: endpoint.host,
                remotePort: 8080
            ) { proxyAddress in
                try await Noora().progressStep(
                    message: "Building container",
                    successMessage: "Container built successfully!",
                    errorMessage: "Failed to build container",
                    showSpinner: true
                ) { _ in
                    try await docker.buildx(name: name, port: proxyAddress?.port ?? 8080)
                }

                try await Noora().progressStep(
                    message: "Uploading container",
                    successMessage: "Container uploaded successfully!",
                    errorMessage: "Failed to upload container",
                    showSpinner: true
                ) { _ in
                    try await docker.push(name: name, port: proxyAddress?.port ?? 8080)
                }
            }

            // TODO: Create image might be needed here, but my tests didn't require it for some reason

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

    struct ContainerdLayer: Sendable {
        enum Source: @unchecked Sendable {
            case path(URL)
            case stream(any AsyncSequence<ArraySlice<UInt8>, any Swift.Error>)
        }

        let source: Source
        let digest: String
        let diffID: String
        let size: Int64
        let gzip: Bool
        let logger = Logger(label: "sh.wendy.cli.run.containerd.layer.stream")

        func withStream(_ write: (ArraySlice<UInt8>) async throws -> Void) async throws {
            switch source {
            case .path(let url):
                logger.debug("Reading layer from path", metadata: ["path": .string(url.path())])
                try await FileSystem.shared.withFileHandle(
                    forReadingAt: FilePath(url.path())
                ) { fileHandle in
                    logger.debug("Reading layer from file handle")
                    for try await chunk in fileHandle.readChunks() {
                        logger.trace(
                            "Reading layer chunk",
                            metadata: ["size": .string("\(chunk.readableBytesView.count) bytes")]
                        )
                        try await write(Array(buffer: chunk)[...])
                    }
                }
            case .stream(let asyncSequence):
                for try await chunk in asyncSequence {
                    try await write(chunk)
                }
            }
        }
    }

    struct DockerManifest: Codable, Sendable {
        let config: String
        let repoTags: [String]
        let layers: [String]
        let layerSources: [String: LayerSource]?

        enum CodingKeys: String, CodingKey {
            case config = "Config"
            case repoTags = "RepoTags"
            case layers = "Layers"
            case layerSources = "LayerSources"
        }
    }

    struct LayerSource: Codable, Sendable {
        let mediaType: String
        let size: Int64
        let digest: String

        enum CodingKeys: String, CodingKey {
            case mediaType = "mediaType"
            case size = "size"
            case digest = "digest"
        }
    }

    struct ContainerConfig: Codable, Sendable {
        let cmd: [String]?
        let env: [String]?
        let workingDir: String?
        let user: String?
        let exposedPorts: [String: [String: String]]?
        let labels: [String: String]?

        enum CodingKeys: String, CodingKey {
            case cmd = "Cmd"
            case env = "Env"
            case workingDir = "WorkingDir"
            case user = "User"
            case exposedPorts = "ExposedPorts"
            case labels = "Labels"
        }
    }

    struct ImageConfig: Codable, Sendable {
        let architecture: String
        let os: String
        let config: ContainerConfig
        let rootfs: RootFS

        enum CodingKeys: String, CodingKey {
            case architecture = "architecture"
            case os = "os"
            case config = "config"
            case rootfs = "rootfs"
        }
    }

    struct RootFS: Codable, Sendable {
        let type: String
        let diffIds: [String]

        enum CodingKeys: String, CodingKey {
            case type = "type"
            case diffIds = "diff_ids"
        }
    }

    func uploadAndRunContainerdContainer(
        layers: [ContainerdLayer],
        imageName: String,
        config: ImageConfig,
        logger: Logger
    ) async throws {
        if layers.isEmpty {
            logger.warning("No layers to run")
            return
        }

        let appConfigData = try await readAppConfigData(logger: logger)

        try await withAgentGRPCClient(
            agentConnectionOptions,
            title: "Which device do you want to run this app on?"
        ) { [appConfigData] client in
            let agentContainers = Wendy_Agent_Services_V1_WendyContainerService.Client(
                wrapping: client
            )
            // TODO: Can we cache this per-device to omit round-trips to the agent?
            logger.debug("Getting existing container layers from agent")
            let existingLayers = try await agentContainers.listLayers(.init()) { response in
                var layers = [Wendy_Agent_Services_V1_LayerHeader]()
                for try await layer in response.messages {
                    layers.append(layer)
                }
                return layers
            }

            let existingHashes = existingLayers.map(\.digest)
            logger.trace("Existing layers: \(existingHashes)")
            logger.trace("Needed layers: \(layers.map(\.digest))")

            logger.debug("Sending changed container layers to agent")
            // Upload layers in parallel
            // This is useful because a stream can only handle one chunk at a time
            // But the networking latency might be high enough over WiFi that we can
            // satisfy the disk more by making more streams. Many streams share a TCP connection
            try await withThrowingTaskGroup { taskGroup in
                actor LayersUploaded {
                    var status = Status()

                    struct Status: Sendable {
                        var layersUploading = 0
                        var layersUploaded = 0
                        var layersFailedUploaded = 0
                        var expectedBytes: Int64 = 0
                        var uploadedBytes: Int64 = 0
                        var progress: Double {
                            return Double(uploadedBytes) / Double(expectedBytes)
                        }

                        var message: String {
                            if layersFailedUploaded > 0 {
                                return
                                    "Layers uploading \(layersUploaded)/\(layersUploading) (failed: \(layersFailedUploaded))"
                            } else {
                                return "Layers uploading \(layersUploaded)/\(layersUploading)"
                            }
                        }
                    }
                    nonisolated let (statusChange, continuation) = AsyncStream<Status>.makeStream(
                        bufferingPolicy: .bufferingNewest(1)
                    )

                    func incrementUploading(_ bytes: Int64) {
                        status.layersUploading += 1
                        status.expectedBytes += bytes
                        continuation.yield(status)
                    }

                    func uploaded(_ bytes: Int64) {
                        status.uploadedBytes += bytes
                        continuation.yield(status)
                        checkFinished()
                    }

                    func incrementUploaded() {
                        status.layersUploaded += 1
                        continuation.yield(status)
                        checkFinished()
                    }

                    func incrementFailedUploaded(error: any Swift.Error) {
                        status.layersFailedUploaded += 1
                        status.layersUploading -= 1
                        continuation.yield(status)
                        checkFinished()
                    }

                    private func checkFinished() {
                        if status.layersUploaded == status.layersUploading {
                            finish()
                        }
                    }

                    nonisolated func finish() {
                        continuation.finish()
                    }

                    deinit {
                        finish()
                    }
                }

                let layersUploaded = LayersUploaded()
                let layersToUpload = layers.filter { !existingHashes.contains($0.digest) }

                if layersToUpload.isEmpty {
                    Noora().info("All layers are already uploaded")
                } else {
                    for layer in layersToUpload {
                        await layersUploaded.incrementUploading(layer.size)
                        taskGroup.addTask {
                            // Upload layers that have changed or are new
                            logger.debug(
                                "Uploading layer to agent",
                                metadata: ["digest": .string(layer.digest)]
                            )
                            do {
                                try await agentContainers.writeLayer(
                                    request: .init { writer in
                                        try await layer.withStream { chunk in
                                            try await writer.write(
                                                .with {
                                                    $0.digest = layer.digest
                                                    $0.data = Data(chunk)
                                                }
                                            )
                                            await layersUploaded.uploaded(Int64(chunk.count))
                                        }
                                    }
                                ) { response in
                                    do {
                                        for try await message in response.messages {
                                            // Ignore responses
                                            logger.trace(
                                                "Got unknown response",
                                                metadata: [
                                                    "digest": .string(layer.digest),
                                                    "response": .string("\(message)"),
                                                ]
                                            )
                                        }
                                    } catch {
                                        logger.error(
                                            "Failed to get response",
                                            metadata: [
                                                "digest": .string(layer.digest),
                                                "error": .string("\(error)"),
                                            ]
                                        )
                                        throw error
                                    }
                                }
                                logger.debug(
                                    "Uploaded layer successfully",
                                    metadata: ["digest": .string(layer.digest)]
                                )
                                await layersUploaded.incrementUploaded()
                            } catch {
                                logger.error(
                                    "Failed to upload layer",
                                    metadata: [
                                        "digest": .string(layer.digest),
                                        "error": .string("\(error)"),
                                    ]
                                )

                                logger.error(
                                    "Failed to upload layer",
                                    metadata: ["error": .string("\(error)")]
                                )
                                await layersUploaded.incrementFailedUploaded(error: error)
                            }
                        }
                    }

                    try await Noora().progressBarStep(message: "Uploading layers to agent") {
                        progress in
                        for await status in layersUploaded.statusChange {
                            progress(status.progress)
                        }

                        let errors = await layersUploaded.status.layersFailedUploaded
                        if errors > 0 {
                            throw Error.failedToUploadLayers(errors)
                        }
                    }
                }

                layersUploaded.finish()

                try await taskGroup.waitForAll()
            }

            logger.debug("Starting container")

            let restartPolicy = computeRestartPolicy()

            _ = try await agentContainers.runContainer(
                .with {
                    $0.imageName = "\(imageName):latest"
                    $0.appName = imageName
                    $0.workingDir = config.config.workingDir ?? "/"
                    $0.cmd = config.config.cmd?.joined(separator: " ") ?? "/bin/\(imageName)"
                    $0.appConfig = appConfigData
                    $0.restartPolicy = restartPolicy
                    $0.layers = layers.map { layer in
                        .with {
                            $0.digest = layer.digest
                            $0.size = layer.size
                            $0.gzip = layer.gzip
                            $0.diffID = layer.diffID
                        }
                    }
                }
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
                    case .none:
                        ()
                    }
                }
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
                    $0.imageName = "\(appName)"
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

    func buildDockerBased(name: String) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.docker.container.build")
        let docker = DockerCLI()
        try await docker.build(name: name)
        logger.debug("Container built successfully!")
    }

    func addSwiftPMResources(
        at buildDir: URL,
        to spec: inout ContainerImageSpec
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.swiftpm-resources")
        let items = try FileManager.default.contentsOfDirectory(
            at: buildDir,
            includingPropertiesForKeys: nil
        )

        var files = [ContainerImageSpec.Layer.File]()

        for item in items where item.lastPathComponent.hasSuffix(".resources") {
            logger.trace(
                "Found resources in build dir",
                metadata: [
                    "path": "\(item.path())"
                ]
            )
            files.append(
                .init(
                    source: item,
                    destination: "/bin/\(item.lastPathComponent)",
                    permissions: 0o700
                )
            )
        }

        if !files.isEmpty {
            logger.debug(
                "Appending resources layer to spec",
                metadata: [
                    "files": .stringConvertible(files.count)
                ]
            )
            spec.layers.append(
                ContainerImageSpec.Layer(files: files)
            )
        }
    }

    func runSwiftContainerdBased() async throws {
        let logger = Logger(label: "sh.wendy.cli.run.containerd")

        let swiftPM = SwiftPM()
        let package = try await swiftPM.dumpPackage(
            .scratchPath(".wendy-build")
        )

        // Get all executable targets
        let executableTargets = package.targets.filter { $0.type == "executable" }

        // Use specified executable or handle multiple executable targets
        let executableTarget: SwiftPM.Package.Target
        if let executableName = executable {
            guard let target = executableTargets.first(where: { $0.name == executableName }) else {
                throw Error.invalidExecutableTarget(executableName)
            }
            executableTarget = target
        } else {
            // If no executable specified, ensure there's only one executable target
            if executableTargets.isEmpty {
                throw Error.noExecutableTarget
            } else if executableTargets.count > 1 {
                throw Error.multipleExecutableTargets(executableTargets.map(\.name))
            } else {
                executableTarget = executableTargets[0]
            }
        }

        let tempDir = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: tempDir)
        }
        let (imageName, container) = try await Noora().progressStep(
            message: "Building container",
            successMessage: "Container built successfully!",
            errorMessage: "Failed to build container",
            showSpinner: true
        ) { progress in
            progress("Building Swift app")
            try await swiftPM.build(
                .product(executableTarget.name),
                .swiftSDK(swiftSDK),
                .configuration(debug ? "debug" : "release"),
                .scratchPath(".wendy-build"),
                .staticSwiftStdlib,
                .xLinker("-s")
            )

            progress("Building container with base image \(baseImage)")
            let binPath = try await swiftPM.buildWithOutput(
                .showBinPath,
                .product(executableTarget.name),
                .swiftSDK(swiftSDK),
                .configuration(debug ? "debug" : "release"),
                .quiet,
                .scratchPath(".wendy-build"),
                .staticSwiftStdlib
            ).trimmingCharacters(in: .whitespacesAndNewlines)
            let buildDir = URL(fileURLWithPath: binPath)
            let executable = buildDir.appendingPathComponent(executableTarget.name)

            logger.debug("Building container with base image \(baseImage)")
            progress("Preparing base image")
            let imageName = executableTarget.name.lowercased()

            // Use the debian:bookworm-slim base image instead of a blank image
            var imageSpec = try await ContainerImageSpec.withBaseImage(
                baseImage: baseImage,
                executable: executable
            )
            progress("Adding Swift PM resources")
            try await addSwiftPMResources(at: buildDir, to: &imageSpec)

            progress("Adding debugger executable")
            if debug {
                // Include the ds2 executable in the container image.
                let ds2URL: URL
                if let url = Bundle.module.url(
                    forResource: "ds2-124963fd-static-linux-arm64",
                    withExtension: nil
                ) {
                    ds2URL = url
                } else {
                    let url = URL(fileURLWithPath: CommandLine.arguments[0])
                        .deletingLastPathComponent()
                        .appending(path: "wendy-agent_wendy.bundle")
                        .appending(path: "Contents")
                        .appending(path: "Resources")
                        .appending(path: "Resources")
                        .appending(component: "ds2-124963fd-static-linux-arm64")

                    guard FileManager.default.fileExists(atPath: url.path()) else {
                        fatalError("Could not find ds2 executable in bundle resources")
                    }

                    ds2URL = url
                }

                let ds2Files = [
                    ContainerImageSpec.Layer.File(
                        source: ds2URL,
                        destination: "/bin/ds2",
                        permissions: 0o755
                    )
                ]
                let ds2Layer = ContainerImageSpec.Layer(files: ds2Files)
                imageSpec.layers.append(ds2Layer)
            }

            progress("Building final container image")
            let container = try await buildDockerContainer(
                image: imageSpec,
                imageName: imageName,
                tempDir: tempDir
            )
            return (imageName, container)
        }

        let cmd: [String]
        if debug {
            cmd = [
                "ds2",
                "gdbserver",
                "0.0.0.0:4242",
                "/bin/\(imageName)",
            ]
        } else {
            // Use the command from the config, or fallback to the image name
            cmd = ["/bin/\(imageName)"]
        }
        // Create a default config for Swift-based containers
        let defaultConfig = ImageConfig(
            architecture: "arm64",
            os: "linux",
            config: ContainerConfig(
                cmd: cmd,
                env: nil,
                workingDir: "/",
                user: nil,
                exposedPorts: nil,
                labels: nil
            ),
            rootfs: RootFS(
                type: "layers",
                diffIds: container.layers.map(\.diffID)
            )
        )

        try await uploadAndRunContainerdContainer(
            layers: container.layers.map { layer in
                ContainerdLayer(
                    source: .path(layer.path),
                    digest: layer.digest,
                    diffID: layer.diffID,
                    size: layer.size,
                    gzip: layer.gzip
                )
            },
            imageName: imageName,
            config: defaultConfig,
            logger: logger
        )
    }

    func runSwiftDockerBased() async throws {
        let logger = Logger(label: "sh.wendy.cli.run.docker.swift")

        let swiftPM = SwiftPM()
        let package = try await swiftPM.dumpPackage(
            .scratchPath(".wendy-build")
        )

        let appConfigData = try await readAppConfigData(logger: logger)

        // Get all executable targets
        let executableTargets = package.targets.filter { $0.type == "executable" }

        // Use specified executable or handle multiple executable targets
        let executableTarget: SwiftPM.Package.Target
        if let executableName = executable {
            guard let target = executableTargets.first(where: { $0.name == executableName }) else {
                throw Error.invalidExecutableTarget(executableName)
            }
            executableTarget = target
        } else {
            // If no executable specified, ensure there's only one executable target
            if executableTargets.isEmpty {
                throw Error.noExecutableTarget
            } else if executableTargets.count > 1 {
                throw Error.multipleExecutableTargets(executableTargets.map(\.name))
            } else {
                executableTarget = executableTargets[0]
            }
        }

        try await swiftPM.build(
            .product(executableTarget.name),
            .swiftSDK(swiftSDK),
            .configuration(debug ? "debug" : "release"),
            .scratchPath(".wendy-build"),
            .staticSwiftStdlib,
            .xLinker("-s")
        )

        let binPath = try await swiftPM.buildWithOutput(
            .showBinPath,
            .product(executableTarget.name),
            .swiftSDK(swiftSDK),
            .configuration(debug ? "debug" : "release"),
            .quiet,
            .scratchPath(".wendy-build"),
            .staticSwiftStdlib
        ).trimmingCharacters(in: .whitespacesAndNewlines)
        let buildDir = URL(fileURLWithPath: binPath)
        let executable = buildDir.appendingPathComponent(executableTarget.name)

        logger.debug("Building container with base image \(baseImage)")
        let imageName = executableTarget.name.lowercased()

        // Use the debian:bookworm-slim base image instead of a blank image
        var imageSpec = try await ContainerImageSpec.withBaseImage(
            baseImage: baseImage,
            executable: executable
        )

        try await addSwiftPMResources(at: buildDir, to: &imageSpec)

        if debug {
            // Include the ds2 executable in the container image.
            let ds2URL: URL
            if let url = Bundle.module.url(
                forResource: "ds2-124963fd-static-linux-arm64",
                withExtension: nil
            ) {
                ds2URL = url
            } else {
                let url = URL(fileURLWithPath: CommandLine.arguments[0])
                    .deletingLastPathComponent()
                    .appending(path: "wendy-agent_wendy.bundle")
                    .appending(path: "Contents")
                    .appending(path: "Resources")
                    .appending(path: "Resources")
                    .appending(component: "ds2-124963fd-static-linux-arm64")

                guard FileManager.default.fileExists(atPath: url.path()) else {
                    fatalError("Could not find ds2 executable in bundle resources")
                }

                ds2URL = url
            }

            let ds2Files = [
                ContainerImageSpec.Layer.File(
                    source: ds2URL,
                    destination: "/bin/ds2",
                    permissions: 0o755
                )
            ]
            let ds2Layer = ContainerImageSpec.Layer(files: ds2Files)
            imageSpec.layers.append(ds2Layer)
        }

        try await uploadDockerTar(
            imageName: imageName,
            appConfigData: appConfigData,
            builtContainer: Task {
                // Wrap the build in a task so we can parallelise starting up the gRPC client
                let outputPath = "\(executableTarget.name)-container.tar"
                try await buildDockerContainerImage(
                    image: imageSpec,
                    imageName: imageName,
                    outputPath: outputPath
                )
                return outputPath
            }
        )
    }

    private func uploadDockerTar(
        imageName: String,
        appConfigData: Data,
        builtContainer: Task<String, any Swift.Error>
    ) async throws {
        let logger = Logger(label: "sh.wendy.cli.run.docker-upload")

        try await withAgentGRPCClient(
            agentConnectionOptions,
            title: "Which device do you want to run this app on?"
        ) { [appConfigData] client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            try await agent.runContainer { writer in
                let outputPath = try await Noora().progressStep(
                    message: "Preparing container for upload",
                    successMessage: "Container prepared for upload",
                    errorMessage: "Failed to prepare container for upload",
                    showSpinner: true
                ) { progress in
                    try await builtContainer.value
                }

                // First, send the header.
                try await writer.write(
                    .with {
                        $0.header.imageName = imageName
                        $0.header.appConfig = appConfigData
                    }
                )

                // Send the chunks
                logger.debug("Uploading app image to agent")
                try await Noora().progressStep(
                    message: "Uploading app image to agent",
                    successMessage: "App image uploaded to agent",
                    errorMessage: "Failed to upload app image to agent",
                    showSpinner: true
                ) { progress in
                    try await FileSystem.shared.withFileHandle(forReadingAt: FilePath(outputPath)) {
                        fileHandle in
                        for try await chunk in fileHandle.readChunks() {
                            try await writer.write(
                                .with {
                                    $0.requestType = .chunk(
                                        .with { $0.data = Data(chunk.readableBytesView) }
                                    )
                                }
                            )
                        }
                    }
                }

                // Send the control command to start the container.
                logger.debug("Sending control command to start container")
                try await Noora().progressStep(
                    message: "Starting app",
                    successMessage: nil,
                    errorMessage: nil,
                    showSpinner: true
                ) { progress in
                    try await writer.write(
                        .with {
                            $0.requestType = .control(
                                .with {
                                    $0.command = .run(
                                        .with {
                                            $0.debug = debug
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
                                }
                            )
                        }
                    )
                }
            } onResponse: { response in
                for try await message in response.messages {
                    switch message.responseType {
                    case .started(let started):
                        if started.debugPort != 0 {
                            Noora().success(
                                "Started container with debug port \(started.debugPort)"
                            )
                        } else {
                            Noora().success("Started container")
                        }
                        if detach {
                            return
                        }
                    case .stopped:
                        Noora().success("Container stopped")
                    case nil:
                        logger.warning("Unknown message received from agent")
                    }
                }
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

    private func writeHTTPBodyToFile(
        body: any AsyncSequence<ArraySlice<UInt8>, any Swift.Error>,
        to url: URL
    ) async throws {
        try await FileSystem.shared.withFileHandle(
            forWritingAt: FilePath(url.path()),
            options: .newFile(replaceExisting: true)
        ) { fileHandle in
            var writer = fileHandle.bufferedWriter()
            for try await chunk in body {
                try await writer.write(contentsOf: chunk)
            }
        }
    }

    private func extractTar(from sourceURL: URL, to destinationURL: URL) async throws {
        _ = try await Subprocess.run(
            .name("tar"),
            arguments: Subprocess.Arguments([
                "-xf", sourceURL.path, "-C", destinationURL.path,
            ]),
            output: .discarded
        )
    }

    private func calculateDigest(for fileURL: URL) async throws -> String {
        return try await FileSystem.shared.withFileHandle(
            forReadingAt: FilePath(fileURL.path)
        ) { fileHandle in
            var sha = SHA256()
            for try await chunk in fileHandle.readChunks() {
                sha.update(data: chunk.readableBytesView)
            }
            let hash = sha.finalize()
                .map { String(format: "%02x", $0) }
                .joined()
            return "sha256:\(hash)"
        }
    }
}
