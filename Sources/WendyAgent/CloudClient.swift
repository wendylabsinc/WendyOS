import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import ServiceLifecycle
import WendyCloudGRPC
import X509

actor CloudClient: Service {
    let transport: HTTP2ClientTransport.Posix
    let logger = Logger(label: "sh.wendy.agent.cloudclient")
    var grpcClient: GRPCClient<HTTP2ClientTransport.Posix>

    init(enrolled: Enrolled, privateKey: Certificate.PrivateKey) throws {
        self.transport = try HTTP2ClientTransport.Posix(
            target: .dns(
                host: enrolled.cloudHost,
                port: 50052
            ),
            transportSecurity: .mTLS(
                certificateChain: enrolled.certificateChainPEM.map { cert in
                    return TLSConfig.CertificateSource.bytes(Array(cert.utf8), format: .pem)
                },
                privateKey: .bytes(
                    Array(privateKey.serializeAsPEM().pemString.utf8),
                    format: .pem
                )
            ) { tls in
                #if DEBUG
                    tls.serverCertificateVerification = .noVerification
                #endif
            }
        )
        self.grpcClient = GRPCClient(transport: transport)
    }

    func updateReleasesFromCloud() async throws {
        let releases = Wendycloud_V1_DeploymentService.Client(wrapping: grpcClient)

        while !Task.isCancelled {
            try await releases.handleReportedState(
                request: StreamingClientRequest<Wendycloud_V1_UpdateReportedStateRequest> {
                    writer in
                    try await Containerd.withClient { client in
                        let tasks = try await client.listTasks()
                        let containers = try await client.listContainers()

                        try await writer.write(
                            .with {
                                $0.currentStates = containers.map { container in
                                    return .with {
                                        $0.appID = container.id
                                        if let appReleaseID = container.labels[
                                            "sh.wendy/app.release.id"
                                        ].flatMap(Int32.init) {
                                            $0.appReleaseID = appReleaseID
                                        }
                                        $0.reportedState =
                                            tasks.first(where: { $0.id == container.id })?.status
                                                == .running ? .running : .stopped
                                        $0.reportedRestartCount =
                                            container.labels["containerd.io/restart.count"].flatMap(
                                                Int32.init
                                            ) ?? Int32(0)
                                        $0.reportedLastExitSignal =
                                            tasks.first(where: { $0.id == container.id })?
                                            .exitStatus ?? 0
                                    }
                                }
                            }
                        )
                        self.logger.info("Updated reported state for containers")
                    }
                }
            ) { response in
                for try await newState in response.messages {
                    self.logger.debug("Received new state: \(newState)")
                }
            }

            try await Task.sleep(for: .seconds(5))
        }
    }

    func run() async throws {
        try await withGracefulShutdownHandler {
            try await withThrowingTaskGroup(of: Void.self) { taskGroup in
                taskGroup.addTask {
                    try await self.grpcClient.runConnections()
                }
                taskGroup.addTask {
                    while !Task.isCancelled {
                        do {
                            try await self.updateReleasesFromCloud()
                        } catch {
                            self.logger.error("Failed to update releases from cloud: \(error)")
                            try await Task.sleep(for: .seconds(5))
                        }
                    }
                }
                try await taskGroup.waitForAll()
            }
        } onGracefulShutdown: {
            Task {
                await self.grpcClient.beginGracefulShutdown()
            }
        }
    }
}
