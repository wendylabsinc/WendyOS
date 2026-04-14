import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCServiceLifecycle
import Logging
import OpenTelemetryGRPC
import ServiceLifecycle
import WendyAgentGRPC

public enum AgentError: LocalizedError, Sendable {
    case stoppedDuringStartup

    public var errorDescription: String? {
        switch self {
        case .stoppedDuringStartup:
            "WendyAgent stopped before startup completed."
        }
    }
}

public actor Agent {
    private enum StartupOutcome {
        case running
    }

    private static let startupProbeDelay: Duration = .milliseconds(300)
    private static let bootstrapLogging: Void = {
        LoggingSystem.bootstrap { label in
            var handler = StreamLogHandler.standardError(label: label)
            handler.logLevel = .info
            return handler
        }
    }()

    public let configuration: AgentConfiguration

    private var serviceGroup: ServiceGroup?
    private var runTask: Task<Void, Error>?

    public init(configuration: AgentConfiguration = .init()) {
        self.configuration = configuration
    }

    public func start() async throws {
        guard self.runTask == nil else { return }

        Self.bootstrapLogging

        let logger = Logger(label: "sh.wendy.agent")
        logger.info("Starting Wendy Agent on port \(self.configuration.port)")

        let broadcaster = TelemetryBroadcaster()

        let docker = DockerCLI()
        let dockerAvailable = await docker.checkAvailable()
        if dockerAvailable {
            logger.info(
                "Docker detected, starting local registry on port \(DockerCLI.registryPort) for Linux container support"
            )
            do {
                try await docker.ensureRegistry()
            } catch {
                logger.warning(
                    "Failed to start Docker registry: \(String(describing: error)). Linux container support disabled."
                )
            }
        } else {
            logger.info("Docker not found, Linux container support disabled")
        }

        let appsBase = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/wendy-agent/apps")

        let services: [any RegistrableRPCService] = [
            AgentService(),
            ContainerService(
                broadcaster: broadcaster,
                executablePath: self.configuration.appPath,
                sandboxProfilePath: self.configuration.sandboxProfile.isEmpty
                    ? nil
                    : self.configuration.sandboxProfile,
                appsBase: appsBase,
                dockerAvailable: dockerAvailable
            ),
            AudioService(),
            ProvisioningService(),
            TelemetryService(broadcaster: broadcaster),
            FileSyncService(appsBase: appsBase),
        ]

        let server = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv6(host: "::", port: self.configuration.port),
                transportSecurity: .plaintext
            ),
            services: services
        )

        let otelServices: [any RegistrableRPCService] = [
            LocalOTelLogsReceiver(broadcaster: broadcaster),
            LocalOTelMetricsReceiver(broadcaster: broadcaster),
            LocalOTelTracesReceiver(broadcaster: broadcaster),
        ]

        let otelServer = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv4(host: "127.0.0.1", port: self.configuration.otelPort),
                transportSecurity: .plaintext
            ),
            services: otelServices
        )

        let bonjour = BonjourAdvertiser(
            port: self.configuration.port,
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
                logger: logger
            )
        )

        let runTask = Task {
            try await serviceGroup.run()
        }

        self.serviceGroup = serviceGroup
        self.runTask = runTask

        logger.info(
            "Listening on port \(self.configuration.port), OTel on port \(self.configuration.otelPort)"
        )

        do {
            try await Self.waitForStartup(of: runTask)
        } catch {
            self.serviceGroup = nil
            self.runTask = nil
            throw error
        }
    }

    public func stop() async {
        let serviceGroup = self.serviceGroup
        let runTask = self.runTask

        self.serviceGroup = nil
        self.runTask = nil

        guard let serviceGroup, let runTask else { return }

        await serviceGroup.triggerGracefulShutdown()
        _ = try? await runTask.value
    }

    private static func waitForStartup(of runTask: Task<Void, Error>) async throws {
        _ = try await withThrowingTaskGroup(of: StartupOutcome.self) { group in
            group.addTask {
                try await runTask.value
                throw AgentError.stoppedDuringStartup
            }
            group.addTask {
                try await Task.sleep(for: Self.startupProbeDelay)
                return .running
            }

            let outcome = try await group.next()!
            group.cancelAll()
            return outcome
        }
    }
}
