import ArgumentParser
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCServiceLifecycle
import Logging
import OpenTelemetryGRPC
import ServiceLifecycle
import WendyAgentGRPC

@main
struct WendyAgent: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "wendy-agent",
        abstract: "Wendy Agent"
    )

    @Option(name: .shortAndLong, help: "The port to listen on for incoming connections.")
    var port: Int = 50051

    @Option(help: "The port to listen on for OpenTelemetry collection.")
    var otelPort: Int = 4317

    @Option(name: .shortAndLong, help: "The directory to store configuration files in.")
    var configDirectory: String = "/etc/wendy-agent"

    @Option(help: "Path to the app executable to run.")
    var appPath: String = ""

    @Option(help: "Path to a sandbox-exec profile to run the app under.")
    var sandboxProfile: String = ""

    func run() async throws {
        LoggingSystem.bootstrap { label in
            var handler = StreamLogHandler.standardError(label: label)
            handler.logLevel = .info
            return handler
        }

        let logger = Logger(label: "sh.wendy.agent")
        logger.info("Starting Wendy Agent on port \(port)")

        let broadcaster = TelemetryBroadcaster()

        // Check Docker availability for Linux container support.
        let docker = DockerCLI()
        let dockerAvailable = await docker.checkAvailable()
        if dockerAvailable {
            logger.info("Docker detected, starting local registry on port \(DockerCLI.registryPort) for Linux container support")
            do {
                try await docker.ensureRegistry()
            } catch {
                logger.warning("Failed to start Docker registry: \(error). Linux container support disabled.")
            }
        } else {
            logger.info("Docker not found, Linux container support disabled")
        }

        let services: [any RegistrableRPCService] = [
            AgentService(),
            ContainerService(
                broadcaster: broadcaster,
                executablePath: appPath,
                sandboxProfilePath: sandboxProfile.isEmpty ? nil : sandboxProfile,
                dockerAvailable: dockerAvailable
            ),
            AudioService(),
            ProvisioningService(),
            TelemetryService(broadcaster: broadcaster),
            FileSyncService(),
        ]

        let server = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv6(host: "::", port: port),
                transportSecurity: .plaintext
            ),
            services: services
        )

        // OTel collector server — receives logs/metrics/traces from local apps
        let otelServices: [any RegistrableRPCService] = [
            LocalOTelLogsReceiver(broadcaster: broadcaster),
            LocalOTelMetricsReceiver(broadcaster: broadcaster),
            LocalOTelTracesReceiver(broadcaster: broadcaster),
        ]

        let otelServer = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv4(host: "127.0.0.1", port: otelPort),
                transportSecurity: .plaintext
            ),
            services: otelServices
        )

        let bonjour = BonjourAdvertiser(
            port: port,
            displayName: ProcessInfo.processInfo.hostName,
            deviceID: ProcessInfo.processInfo.hostName
        )

        let serviceGroup = ServiceGroup(
            configuration: .init(
                services: [
                    .init(service: server),
                    .init(service: otelServer),
                    .init(service: bonjour),
                ],
                gracefulShutdownSignals: [.sigterm],
                cancellationSignals: [.sigint],
                logger: logger
            )
        )

        logger.info("Listening on port \(port), OTel on port \(otelPort)")
        try await serviceGroup.run()
    }
}
