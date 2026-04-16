import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCServiceLifecycle
import Logging
import OpenTelemetryGRPC
import ServiceLifecycle
import WendyAgentGRPC

@MainActor
public final class WendyAgent {
    public let configuration: WendyAgentConfiguration
    public private(set) var status: WendyAgentStatus = .idle

    public init(configuration: WendyAgentConfiguration = .init()) {
        self.configuration = configuration
    }

    public func start() async throws {
        guard self.runTask == nil else { return }

        Self.bootstrapLogging
        self.updateStatus(.starting)

        let logger = Logger(label: "sh.wendy.agent")
        logger.info(
            "Starting Wendy Agent",
            metadata: [
                "grpc_port": "\(self.configuration.port)",
                "otel_port": "\(self.configuration.otelPort)",
            ]
        )

        var startupStage = "telemetry broadcaster initialization"
        logger.info("Startup stage: \(startupStage)")
        let broadcaster = TelemetryBroadcaster()

        let docker = DockerCLI()
        startupStage = "Docker availability probe"
        logger.info("Startup stage: \(startupStage)")
        let dockerAvailable = await docker.checkAvailable()
        if dockerAvailable {
            startupStage = "Docker local registry startup"
            logger.info(
                "Startup stage: \(startupStage)",
                metadata: ["registry_port": "\(DockerCLI.registryPort)"]
            )
            do {
                try await docker.ensureRegistry()
                logger.info("Startup stage complete: \(startupStage)")
            } catch {
                logger.warning(
                    "Failed to start Docker registry: \(String(describing: error)). Linux container support disabled."
                )
            }
        } else {
            logger.info("Docker not available, Linux container support disabled")
        }

        startupStage = "application support path setup"
        logger.info("Startup stage: \(startupStage)")
        let appsBase = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/wendy-agent/apps")

        startupStage = "main Wendy Agent gRPC service construction"
        logger.info("Startup stage: \(startupStage)")
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

        startupStage = "main Wendy Agent gRPC server creation"
        logger.info(
            "Startup stage: \(startupStage)",
            metadata: ["grpc_port": "\(self.configuration.port)"]
        )
        let server = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv6(host: "::", port: self.configuration.port),
                transportSecurity: .plaintext
            ),
            services: services
        )

        startupStage = "local OpenTelemetry gRPC service construction"
        logger.info("Startup stage: \(startupStage)")
        let otelServices: [any RegistrableRPCService] = [
            LocalOTelLogsReceiver(broadcaster: broadcaster),
            LocalOTelMetricsReceiver(broadcaster: broadcaster),
            LocalOTelTracesReceiver(broadcaster: broadcaster),
        ]

        startupStage = "local OpenTelemetry gRPC server creation"
        logger.info(
            "Startup stage: \(startupStage)",
            metadata: ["otel_port": "\(self.configuration.otelPort)"]
        )
        let otelServer = GRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv4(host: "127.0.0.1", port: self.configuration.otelPort),
                transportSecurity: .plaintext
            ),
            services: otelServices
        )

        startupStage = "Bonjour advertiser creation"
        logger.info("Startup stage: \(startupStage)")
        let bonjour = BonjourAdvertiser(
            port: self.configuration.port,
            displayName: ProcessInfo.processInfo.hostName,
            deviceID: ProcessInfo.processInfo.hostName
        )

        startupStage = "service group creation"
        logger.info("Startup stage: \(startupStage)")
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

        startupStage = "service group launch"
        logger.info("Startup stage: \(startupStage)")
        let runTask = Self.makeRunTask(serviceGroup: serviceGroup)

        self.runIdentifier &+= 1
        let runIdentifier = self.runIdentifier
        self.serviceGroup = serviceGroup
        self.runTask = runTask
        self.runTaskMonitor?.cancel()
        self.runTaskMonitor = Task {
            await self.monitorRunTask(runTask, runIdentifier: runIdentifier)
        }

        logger.info(
            "Listening on port \(self.configuration.port), OTel on port \(self.configuration.otelPort)"
        )

        startupStage = "status transition to running"
        logger.info("Startup stage: \(startupStage)")
        guard self.runIdentifier == runIdentifier, self.runTask != nil else { return }
        self.updateStatus(.running)
        logger.info("Startup complete: Wendy Agent is running")
    }

    public func stop() async {
        guard let serviceGroup, let runTask else {
            return
        }

        self.updateStatus(.stopping)

        await serviceGroup.triggerGracefulShutdown()

        do {
            try await runTask.value
            self.finalizeRunTaskIfNeeded(runIdentifier: self.runIdentifier, error: nil)
        } catch {
            self.finalizeRunTaskIfNeeded(runIdentifier: self.runIdentifier, error: error)
        }
    }

    public func observeStatus(
        _ handler: @escaping @isolated(any) @Sendable (WendyAgentStatus) -> Void
    ) -> WendyObservation {
        let observationID = self.statusObservationRegistry.register(handler, initialValue: self.status)
        self.scheduleStatusObservation(for: observationID)

        return WendyObservation { [self] in
            await self.cancelStatusObservation(for: observationID)
        }
    }

    // MARK: - Private

    private static let bootstrapLogging: Void = {
        LoggingSystem.bootstrap { label in
            var handler = StreamLogHandler.standardError(label: label)
            handler.logLevel = .info
            return handler
        }
    }()

    private var serviceGroup: ServiceGroup?
    private var runTask: Task<Void, Error>?
    private var runTaskMonitor: Task<Void, Never>?
    private var runIdentifier: UInt64 = 0
    private var statusObservationRegistry = WendyObservationRegistry<WendyAgentStatus>(areEquivalent: ==)
    private var statusObservationTasks: [WendyObservationRegistry<WendyAgentStatus>.ObservationID: Task<Void, Never>] = [:]

    private func monitorRunTask(_ runTask: Task<Void, Error>, runIdentifier: UInt64) async {
        do {
            try await runTask.value
            self.finalizeRunTaskIfNeeded(runIdentifier: runIdentifier, error: nil)
        } catch {
            self.finalizeRunTaskIfNeeded(runIdentifier: runIdentifier, error: error)
        }
    }

    private func finalizeRunTaskIfNeeded(runIdentifier: UInt64, error: (any Error)?) {
        guard self.runIdentifier == runIdentifier, self.runTask != nil else { return }

        self.serviceGroup = nil
        self.runTask = nil
        self.runTaskMonitor?.cancel()
        self.runTaskMonitor = nil

        switch self.status {
        case .stopping:
            self.updateStatus(.stopped)
        case .starting, .running:
            if let error {
                self.updateStatus(.failed(Self.errorMessage(for: error)))
            } else {
                self.updateStatus(.stopped)
            }
        case .idle, .stopped, .failed:
            break
        }
    }

    private func updateStatus(_ status: WendyAgentStatus) {
        self.status = status

        let observationIDs = self.statusObservationRegistry.enqueue(status)
        for observationID in observationIDs {
            self.scheduleStatusObservation(for: observationID)
        }
    }

    private func scheduleStatusObservation(
        for observationID: WendyObservationRegistry<WendyAgentStatus>.ObservationID
    ) {
        guard self.statusObservationTasks[observationID] == nil else { return }

        let task = Task { @MainActor in
            await self.runStatusObservation(for: observationID)
        }
        self.statusObservationTasks[observationID] = task
    }

    private func runStatusObservation(
        for observationID: WendyObservationRegistry<WendyAgentStatus>.ObservationID
    ) async {
        while let delivery = self.statusObservationRegistry.beginDelivery(for: observationID) {
            await delivery.handler(delivery.value)

            let shouldContinue = self.statusObservationRegistry.finishDelivery(
                for: observationID,
                delivered: delivery.value
            )
            guard shouldContinue else { break }
        }

        self.statusObservationTasks.removeValue(forKey: observationID)
    }

    private func cancelStatusObservation(
        for observationID: WendyObservationRegistry<WendyAgentStatus>.ObservationID
    ) async {
        self.statusObservationRegistry.removeObservation(observationID)
        let task = self.statusObservationTasks.removeValue(forKey: observationID)
        await task?.value
    }

    nonisolated private static func makeRunTask(serviceGroup: ServiceGroup) -> Task<Void, Error> {
        Task {
            try await serviceGroup.run()
        }
    }

    private static func errorMessage(for error: any Error) -> String {
        if let localizedError = error as? LocalizedError,
           let description = localizedError.errorDescription,
           !description.isEmpty
        {
            return description
        }

        let description = String(describing: error)
        return description.isEmpty ? "WendyAgent failed to start." : description
    }
}
