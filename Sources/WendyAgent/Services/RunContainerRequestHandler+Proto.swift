import WendyAgentGRPC

extension RunContainerRequestHandler.Header {
    /// Initialize a header from a protobuf header, validating the contents.
    init(validating proto: Wendy_Agent_Services_V1_RunContainerRequest.Header) throws {
        guard !proto.imageName.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "Image name cannot be empty")
        }

        self.imageName = proto.imageName
        self.appConfig = proto.appConfig
    }
}

extension RunContainerRequestHandler.Chunk {
    init(validating proto: Wendy_Agent_Services_V1_RunContainerRequest.Chunk) throws {
        guard !proto.data.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "Chunk data cannot be empty")
        }

        self.data = proto.data
    }
}

extension RunContainerRequestHandler.ControlCommand {
    init(validating proto: Wendy_Agent_Services_V1_ControlCommand) throws {
        switch proto.command {
        case .run(let run):
            var restart: Run.RestartPolicy = .default
            switch run.restartPolicy.mode {
            case .unlessStopped:
                restart = .unlessStopped
            case .no:
                restart = .no
            case .onFailure:
                let retries = Int(run.restartPolicy.onFailureMaxRetries)
                restart = .onFailure(max(0, retries))
            case .default:
                restart = .default
            case .UNRECOGNIZED:
                restart = .default
            }
            self = .run(Run(debug: run.debug, restartPolicy: restart))
        case .stop:
            self = .stop
        case nil:
            throw RPCError(code: .invalidArgument, message: "Control command cannot be unspecified")
        }
    }
}

extension RunContainerRequestHandler.Event {
    var proto: Wendy_Agent_Services_V1_RunContainerResponse {
        .with {
            switch self {
            case .containerStarted(let containerStarted):
                $0.responseType = .started(
                    .with {
                        $0.debugPort = containerStarted.debugPort
                    }
                )
            case .containerStopped:
                $0.responseType = .stopped(.init())
            case .consoleOutput(let output):
                switch output.type {
                case .stdout:
                    $0.responseType = .stdoutOutput(
                        .with {
                            $0.data = output.data
                        }
                    )
                case .stderr:
                    $0.responseType = .stderrOutput(
                        .with {
                            $0.data = output.data
                        }
                    )
                }
            }
        }
    }
}
