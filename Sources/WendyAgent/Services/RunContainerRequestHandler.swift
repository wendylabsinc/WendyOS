import AppConfig
import Foundation
import Logging
import NIOFileSystem
import Subprocess

/// A state machine that handles the request to run a container.
struct RunContainerRequestHandler {
    enum State {
        /// This is the initial state. The handler is waiting for the header.
        case waitingForHeader

        /// After the header is received, the handler transitions to the `acceptingChunks`. In this
        /// state, a file handle is opened for writing and chunks are being accepted.
        case acceptingChunks(AcceptingChunks)

        /// Container is running, with associated data about the running container.
        case running(Running)

        struct AcceptingChunks {
            let header: Header
            var writer: BufferedWriter<WriteFileHandle>
            var imagePath: FilePath
            var fileHandle: WriteFileHandle
        }

        struct Running {
            let imageName: String
            let debugPort: UInt32
        }
    }

    /// The header of the request.
    struct Header {
        let imageName: String
        let appConfig: Data
    }

    struct Chunk {
        let data: Data
    }

    enum ControlCommand {
        case run(Run)
        case stop

        struct Run {
            var debug: Bool
            enum RestartPolicy {
                case `default`
                case unlessStopped
                case no
                case onFailure(Int)
            }
            var restartPolicy: RestartPolicy = .default
        }
    }

    enum Event {
        case containerStarted(ContainerStarted)
        case containerStopped

        struct ContainerStarted {
            let debugPort: UInt32
        }
    }

    enum Error: Swift.Error {
        /// A message was received before the header.
        case expectedHeader

        /// A header message was received, but not expected.
        case unexpectedHeader

        /// A chunk message was received, but not expected.
        case unexpectedChunk

        /// An internal inconsistency was detected. This is a programming error in the agent.
        case internalInconsistency

        /// An unexpected control command was received.
        case unexpectedControlCommand(ControlCommand)

        /// The container failed to start.
        case containerStartFailed(Swift.Error)
    }

    public let events: AsyncStream<Event>
    private let eventsContinuation: AsyncStream<Event>.Continuation

    private var state: State = .waitingForHeader
    private let dockerCLI = DockerCLI()
    private let logger = Logger(label: "sh.wendy-agent.run-container")

    init() {
        let (stream, continuation) = AsyncStream<Event>.makeStream(bufferingPolicy: .unbounded)
        self.events = stream
        self.eventsContinuation = continuation
    }

    func cleanup() async {
        switch state {
        case .acceptingChunks(let acceptingChunks):
            try? await acceptingChunks.fileHandle.close()
        case .waitingForHeader, .running:
            ()
        }
    }

    mutating func handle(_ header: Header) async throws {
        guard case .waitingForHeader = self.state else {
            throw Error.unexpectedHeader
        }

        // Create a file for writing in the temporary directory.
        let uuid = UUID().uuidString
        let fileName = "container-\(header.imageName).\(uuid).tar"
        let path = try await FileSystem.shared.temporaryDirectory.appending(fileName)
        logger.info("Writing container image", metadata: ["path": .string(path.string)])
        let writeHandle = try await FileSystem.shared.openFile(
            forWritingAt: path,
            options: .newFile(replaceExisting: false)
        )
        let writer = writeHandle.bufferedWriter()

        self.state = .acceptingChunks(
            State.AcceptingChunks(
                header: header,
                writer: writer,
                imagePath: path,
                fileHandle: writeHandle
            )
        )
    }

    mutating func handle(_ chunk: Chunk) async throws {
        guard case .acceptingChunks(var state) = self.state else {
            throw Error.unexpectedChunk
        }

        logger.debug("Writing chunk", metadata: ["size": .string("\(chunk.data.count) bytes")])
        try await state.writer.write(contentsOf: chunk.data)
        self.state = .acceptingChunks(state)
    }

    mutating func handle(_ control: ControlCommand) async throws {
        switch (state, control) {
        case (.waitingForHeader, _):
            throw Error.expectedHeader

        case (.acceptingChunks(var acceptingState), .run(let run)):
            // Finalize writing the container image
            try await acceptingState.writer.flush()
            try await acceptingState.fileHandle.close()

            // Load the container image into Docker
            let imagePath = acceptingState.imagePath.string
            logger.info(
                "Loading container image into Docker",
                metadata: ["path": .string(imagePath)]
            )
            try await dockerCLI.load(filePath: imagePath)

            let imageName = acceptingState.header.imageName
            let containerName = "container-\(imageName)"

            // Stop and remove any existing container with this name
            logger.info(
                "Stopping any existing container with the same name",
                metadata: ["container": .string(containerName)]
            )
            do {
                _ = try await dockerCLI.stop(container: containerName, timeoutSeconds: 10)
                logger.info(
                    "Stopped existing container",
                    metadata: ["container": .string(containerName)]
                )
            } catch {
                logger.debug(
                    "No running container to stop or stop failed",
                    metadata: ["container": .string(containerName), "error": .string("\(error)")]
                )
            }
            logger.info(
                "Removing any existing containers with the same name",
                metadata: ["container": .string(containerName)]
            )
            try await dockerCLI.rm(options: [.force], container: containerName)

            var runOptions: [DockerCLI.RunOption] = [
                .name(containerName),
                .detach,
                .privileged,  // Add privileged access for all containers
            ]

            do {
                let appConfig = try JSONDecoder().decode(
                    AppConfig.self,
                    from: acceptingState.header.appConfig
                )
                for entitlement in appConfig.entitlements {
                    switch entitlement {
                    case .gpu:
                        runOptions.append(.gpus("all"))
                    case .video:
                        runOptions.append(.device("/dev/video0"))
                    case .bluetooth:
                        runOptions.append(.capAdd("NET_ADMIN"))
                        runOptions.append(.capAdd("NET_RAW"))
                    case .audio:
                        runOptions.append(.device("/dev/snd/"))
                    case .network(let network):
                        switch network.mode {
                        case .host:
                            runOptions.append(.network("host"))
                        case .none:
                            runOptions.append(.network("none"))
                        }
                    }
                }
            } catch {
                logger.error(
                    "Failed to decode app config",
                    metadata: ["error": .string("\(error)")]
                )
            }

            // Apply restart policy overrides or defaults
            switch run.restartPolicy {
            case .unlessStopped:
                runOptions.append(.restartUnlessStopped)
            case .no:
                runOptions.append(.restartNo)
            case .onFailure(let retries):
                runOptions.append(.restartOnFailure(retries))
            case .default:
                // Default: no restart when debugging, unless-stopped otherwise
                runOptions.append(run.debug ? .restartNo : .restartUnlessStopped)
            }

            var debugPort: UInt32 = 0

            if run.debug {
                // Configure for debugging
                debugPort = 4242
                logger.info(
                    "Starting container in debug mode",
                    metadata: ["image": .string(imageName), "port": .string("\(debugPort)")]
                )
                runOptions.append(contentsOf: [
                    .capAdd("SYS_PTRACE"),
                    .securityOpt("seccomp=unconfined"),
                ])

                do {
                    try await dockerCLI.run(
                        options: runOptions,
                        image: imageName,
                        command: ["ds2", "gdbserver", "0.0.0.0:\(debugPort)", "/bin/\(imageName)"]
                    )
                    logger.info(
                        "Container started in debug mode successfully",
                        metadata: ["image": .string(imageName)]
                    )
                } catch {
                    logger.error(
                        "Failed to start container in debug mode",
                        metadata: ["error": .string("\(error)")]
                    )
                    throw Error.containerStartFailed(error)
                }
            } else {
                // Start the container without debugging
                logger.info(
                    "Starting container without debugging",
                    metadata: ["image": .string(imageName)]
                )
                try await dockerCLI.run(options: runOptions, image: imageName)
                logger.info(
                    "Container started successfully",
                    metadata: ["image": .string(imageName)]
                )
            }

            eventsContinuation.yield(.containerStarted(.init(debugPort: debugPort)))

            // Update state to running
            self.state = .running(
                State.Running(
                    imageName: imageName,
                    debugPort: debugPort
                )
            )

        case (.acceptingChunks(let acceptingState), .stop):
            // Stop any running container with this image name
            let imageName = acceptingState.header.imageName
            let containerName = "container-\(imageName)"
            logger.info(
                "Stopping container on request",
                metadata: ["container": .string(containerName)]
            )
            do {
                _ = try await dockerCLI.stop(container: containerName, timeoutSeconds: 10)
                logger.info(
                    "Container stopped",
                    metadata: ["container": .string(containerName)]
                )
                eventsContinuation.yield(.containerStopped)
            } catch {
                logger.error(
                    "Failed to stop container",
                    metadata: ["container": .string(containerName), "error": .string("\(error)")]
                )
                throw error
            }
        // Keep state; caller may still upload/run a new image
        case (.running(let running), .stop):
            let containerName = "container-\(running.imageName)"
            logger.info(
                "Stopping running container on request",
                metadata: ["container": .string(containerName)]
            )
            do {
                _ = try await dockerCLI.stop(container: containerName, timeoutSeconds: 10)
                logger.info(
                    "Container stopped",
                    metadata: ["container": .string(containerName)]
                )
                eventsContinuation.yield(.containerStopped)
            } catch {
                logger.error(
                    "Failed to stop container",
                    metadata: ["container": .string(containerName), "error": .string("\(error)")]
                )
                throw error
            }
        case (.running, _):
            throw Error.unexpectedControlCommand(control)
        }
    }
}
