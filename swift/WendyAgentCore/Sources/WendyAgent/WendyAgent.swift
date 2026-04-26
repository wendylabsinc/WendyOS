import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import OpenTelemetryGRPC
import WendyAgentGRPC

@MainActor
public final class WendyAgent {
    private typealias PosixGRPCServer = GRPCServer<HTTP2ServerTransport.Posix>

    public let configuration: WendyAgentConfiguration
    public private(set) var status: WendyAgentStatus = .idle
    public private(set) var apps: [WendyAppInfo] = []

    public init(configuration: WendyAgentConfiguration = .init()) {
        self.configuration = configuration
    }

    public func start() async throws {
        switch self.status {
        case .idle, .stopped, .failed:
            break
        case .starting, .running, .stopping:
            return
        }

        Self.bootstrapLogging
        self.updateStatus(.starting)
        self.logger.info(
            "Starting Wendy Agent",
            metadata: [
                "grpc_port": "\(self.configuration.port)",
                "otel_port": "\(self.configuration.otelPort)",
            ]
        )

        let broadcaster = TelemetryBroadcaster()

        do {
            let dockerAvailability = await self.prepareDockerIfNeeded()

            try await self.startMainServer(
                dockerAvailability: dockerAvailability,
                broadcaster: broadcaster
            )
            try await self.startOTelServer(broadcaster: broadcaster)
            try await self.startBonjour()

            self.runIdentifier &+= 1
            self.handlingUnexpectedRuntimeExit = false
            self.startMonitorTask(runIdentifier: self.runIdentifier)

            self.updateStatus(.running)
            self.logger.info(
                "Wendy Agent is running",
                metadata: [
                    "grpc_port": "\(self.configuration.port)",
                    "otel_port": "\(self.configuration.otelPort)",
                ]
            )
        } catch {
            await self.rollbackStartup()
            self.clearRuntimeState()
            self.updateStatus(.failed(Self.errorMessage(for: error)))
            throw error
        }
    }

    public func stop() async {
        guard case .running = self.status else {
            return
        }

        self.logger.info("Stopping Wendy Agent")
        self.updateStatus(.stopping)
        self.stopMonitorTask()

        await self.containerService?.beginStopping()
        await self.containerService?.stopAllApps()

        await self.stopBonjour()
        await self.stopOTelServer()
        await self.stopMainServer()

        self.clearRuntimeState()
        self.updateStatus(.stopped)
        self.logger.info("Wendy Agent stopped")
    }

    public func observeStatus(
        _ handler: @escaping @isolated(any) @Sendable (WendyAgentStatus) -> Void
    ) -> WendyObservation {
        let observationID = self.statusObservationRegistry.register(
            handler,
            initialValue: self.status
        )
        self.scheduleStatusObservation(for: observationID)

        return WendyObservation { [self] in
            await self.cancelStatusObservation(for: observationID)
        }
    }

    public func observeApps(
        _ handler: @escaping @isolated(any) @Sendable ([WendyAppInfo]) -> Void
    ) -> WendyObservation {
        let observationID = self.appsObservationRegistry.register(handler, initialValue: self.apps)
        self.scheduleAppsObservation(for: observationID)

        return WendyObservation { [self] in
            await self.cancelAppsObservation(for: observationID)
        }
    }

    public func stopApp(id: String) async {
        await self.containerService?.stopApp(id: id)
    }

    public func stopAllApps() async {
        await self.containerService?.stopAllApps()
    }

    // MARK: - Private

    private static let bootstrapLogging: Void = {
        LoggingSystem.bootstrap { label in
            var handler = StreamLogHandler.standardError(label: label)
            handler.logLevel = .info
            return handler
        }
    }()

    private let logger = Logger(label: "sh.wendy.agent")

    private var mainServer: PosixGRPCServer?
    private var mainServerTask: Task<Void, Error>?
    private var containerService: ContainerService?

    private var otelServer: PosixGRPCServer?
    private var otelServerTask: Task<Void, Error>?

    private var bonjourRegistration: BonjourRegistration?
    private var bonjourTask: Task<Void, Error>?

    private var monitorTask: Task<Void, Never>?
    private var runIdentifier: UInt64 = 0
    private var handlingUnexpectedRuntimeExit = false
    private var statusObservationRegistry = WendyObservationRegistry<WendyAgentStatus>(
        areEquivalent: ==
    )
    private var statusObservationTasks:
        [WendyObservationRegistry<WendyAgentStatus>.ObservationID: Task<Void, Never>] = [:]
    private var appsObservationRegistry = WendyObservationRegistry<[WendyAppInfo]>(
        areEquivalent: ==
    )
    private var appsObservationTasks:
        [WendyObservationRegistry<[WendyAppInfo]>.ObservationID: Task<Void, Never>] = [:]
    private static let linuxContainersUnsupportedMessage =
        "Linux containers aren't supported on Macs yet. Support is planned for a future release."

    private func prepareDockerIfNeeded() async -> DockerCLI.AvailabilityCheckResult {
        self.logger.info(
            "Linux container support disabled on macOS",
            metadata: ["reason": "\(Self.linuxContainersUnsupportedMessage)"]
        )
        return DockerCLI.AvailabilityCheckResult(
            isAvailable: false,
            failureMessage: Self.linuxContainersUnsupportedMessage
        )
    }

    private func startMainServer(
        dockerAvailability: DockerCLI.AvailabilityCheckResult,
        broadcaster: TelemetryBroadcaster
    ) async throws {
        let stateDirectory = WendyAgentPaths.stateDirectory
        let appsBase = stateDirectory.appendingPathComponent("apps")

        let containerService = ContainerService(
            broadcaster: broadcaster,
            executablePath: self.configuration.appPath,
            sandboxProfilePath: self.configuration.sandboxProfile.isEmpty
                ? nil
                : self.configuration.sandboxProfile,
            stateDirectory: stateDirectory,
            appsBase: appsBase,
            dockerAvailable: dockerAvailability.isAvailable,
            dockerUnavailableMessage: dockerAvailability.failureMessage,
            onAppsChanged: { [weak self] apps in
                await self?.updateApps(apps)
            }
        )
        self.containerService = containerService
        await containerService.publishCurrentApps()

        let services: [any RegistrableRPCService] = [
            AgentService(),
            containerService,
            AudioService(),
            ProvisioningService(),
            TelemetryService(broadcaster: broadcaster),
            FileSyncService(appsBase: appsBase),
        ]

        let server = PosixGRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv6(host: "::", port: self.configuration.port),
                transportSecurity: .plaintext,
                config: .defaults {
                    $0.http2.maxFrameSize = 256 * 1024
                    $0.http2.targetWindowSize = 8 * 1024 * 1024
                    $0.rpc.maxRequestPayloadSize = 16 * 1024 * 1024
                }
            ),
            services: services
        )

        let task = Self.makeServeTask(server: server)

        do {
            if let address = try await server.listeningAddress {
                self.logger.info(
                    "Main Wendy Agent gRPC server listening",
                    metadata: ["grpc_address": "\(address)"]
                )
            } else {
                self.logger.info("Main Wendy Agent gRPC server listening")
            }

            self.mainServer = server
            self.mainServerTask = task
        } catch {
            server.beginGracefulShutdown()
            _ = try? await task.value
            throw error
        }
    }

    private func stopMainServer() async {
        self.mainServer?.beginGracefulShutdown()
        _ = try? await self.mainServerTask?.value
        self.mainServer = nil
        self.mainServerTask = nil
    }

    private func startOTelServer(
        broadcaster: TelemetryBroadcaster
    ) async throws {
        let services: [any RegistrableRPCService] = [
            LocalOTelLogsReceiver(broadcaster: broadcaster),
            LocalOTelMetricsReceiver(broadcaster: broadcaster),
            LocalOTelTracesReceiver(broadcaster: broadcaster),
        ]

        let server = PosixGRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv4(host: "127.0.0.1", port: self.configuration.otelPort),
                transportSecurity: .plaintext
            ),
            services: services
        )

        let task = Self.makeServeTask(server: server)

        do {
            if let address = try await server.listeningAddress {
                self.logger.info(
                    "Local OpenTelemetry gRPC server listening",
                    metadata: ["otel_address": "\(address)"]
                )
            } else {
                self.logger.info("Local OpenTelemetry gRPC server listening")
            }

            self.otelServer = server
            self.otelServerTask = task
        } catch {
            server.beginGracefulShutdown()
            _ = try? await task.value
            throw error
        }
    }

    private func stopOTelServer() async {
        self.otelServer?.beginGracefulShutdown()
        _ = try? await self.otelServerTask?.value
        self.otelServer = nil
        self.otelServerTask = nil
    }

    private func startBonjour() async throws {
        let advertiser = BonjourAdvertiser(
            port: self.configuration.port,
            displayName: ProcessInfo.processInfo.hostName,
            deviceID: ProcessInfo.processInfo.hostName
        )

        let runtime = try await advertiser.start()
        self.logger.info("Bonjour advertisement registered")

        self.bonjourRegistration = runtime.registration
        self.bonjourTask = runtime.task
    }

    private func stopBonjour() async {
        await self.bonjourRegistration?.shutdown()
        _ = try? await self.bonjourTask?.value
        self.bonjourRegistration = nil
        self.bonjourTask = nil
    }

    private func startMonitorTask(runIdentifier: UInt64) {
        guard let mainServerTask = self.mainServerTask,
            let otelServerTask = self.otelServerTask,
            let bonjourTask = self.bonjourTask
        else {
            return
        }

        self.stopMonitorTask()
        self.monitorTask = Self.makeMonitorTask(
            agent: self,
            mainServerTask: mainServerTask,
            otelServerTask: otelServerTask,
            bonjourTask: bonjourTask,
            runIdentifier: runIdentifier
        )
    }

    private func stopMonitorTask() {
        self.monitorTask?.cancel()
        self.monitorTask = nil
    }

    private func rollbackStartup() async {
        await self.stopBonjour()
        await self.stopOTelServer()
        await self.stopMainServer()
    }

    private func clearRuntimeState() {
        self.mainServer = nil
        self.mainServerTask = nil
        self.containerService = nil
        self.otelServer = nil
        self.otelServerTask = nil
        self.bonjourRegistration = nil
        self.bonjourTask = nil
        self.stopMonitorTask()
        self.handlingUnexpectedRuntimeExit = false
    }

    private func monitorRuntimeTasks(
        mainServerTask: Task<Void, Error>,
        otelServerTask: Task<Void, Error>,
        bonjourTask: Task<Void, Error>,
        runIdentifier: UInt64
    ) async {
        await withTaskGroup(of: Void.self) { group in
            group.addTask {
                await self.monitorRuntimeTask(
                    mainServerTask,
                    subsystem: "main_grpc",
                    runIdentifier: runIdentifier
                )
            }
            group.addTask {
                await self.monitorRuntimeTask(
                    otelServerTask,
                    subsystem: "otel_grpc",
                    runIdentifier: runIdentifier
                )
            }
            group.addTask {
                await self.monitorRuntimeTask(
                    bonjourTask,
                    subsystem: "bonjour",
                    runIdentifier: runIdentifier
                )
            }

            await group.next()
            group.cancelAll()
        }
    }

    private func monitorRuntimeTask(
        _ task: Task<Void, Error>,
        subsystem: String,
        runIdentifier: UInt64
    ) async {
        do {
            try await task.value
            guard !Task.isCancelled else { return }
            await self.handleUnexpectedRuntimeExit(
                subsystem: subsystem,
                error: nil,
                runIdentifier: runIdentifier
            )
        } catch is CancellationError {
            return
        } catch {
            await self.handleUnexpectedRuntimeExit(
                subsystem: subsystem,
                error: error,
                runIdentifier: runIdentifier
            )
        }
    }

    private func handleUnexpectedRuntimeExit(
        subsystem: String,
        error: (any Error)?,
        runIdentifier: UInt64
    ) async {
        guard !Task.isCancelled,
            !self.handlingUnexpectedRuntimeExit,
            self.runIdentifier == runIdentifier,
            case .running = self.status
        else {
            return
        }

        self.handlingUnexpectedRuntimeExit = true

        if let error {
            self.logger.error(
                "Runtime subsystem stopped unexpectedly",
                metadata: [
                    "subsystem": "\(subsystem)",
                    "error": "\(Self.errorMessage(for: error))",
                ]
            )
        } else {
            self.logger.warning(
                "Runtime subsystem stopped unexpectedly",
                metadata: ["subsystem": "\(subsystem)"]
            )
        }

        await self.stopBonjour()
        await self.stopOTelServer()
        await self.stopMainServer()
        self.clearRuntimeState()

        if let error {
            self.updateStatus(.failed(Self.errorMessage(for: error)))
        } else {
            self.updateStatus(.stopped)
        }
    }

    private func updateStatus(_ status: WendyAgentStatus) {
        self.status = status

        let observationIDs = self.statusObservationRegistry.enqueue(status)
        for observationID in observationIDs {
            self.scheduleStatusObservation(for: observationID)
        }
    }

    private func updateApps(_ apps: [WendyAppInfo]) {
        guard self.apps != apps else { return }

        self.apps = apps

        let observationIDs = self.appsObservationRegistry.enqueue(apps)
        for observationID in observationIDs {
            self.scheduleAppsObservation(for: observationID)
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

    private func scheduleAppsObservation(
        for observationID: WendyObservationRegistry<[WendyAppInfo]>.ObservationID
    ) {
        guard self.appsObservationTasks[observationID] == nil else { return }

        let task = Task { @MainActor in
            await self.runAppsObservation(for: observationID)
        }
        self.appsObservationTasks[observationID] = task
    }

    private func runAppsObservation(
        for observationID: WendyObservationRegistry<[WendyAppInfo]>.ObservationID
    ) async {
        while let delivery = self.appsObservationRegistry.beginDelivery(for: observationID) {
            await delivery.handler(delivery.value)

            let shouldContinue = self.appsObservationRegistry.finishDelivery(
                for: observationID,
                delivered: delivery.value
            )
            guard shouldContinue else { break }
        }

        self.appsObservationTasks.removeValue(forKey: observationID)
    }

    private func cancelAppsObservation(
        for observationID: WendyObservationRegistry<[WendyAppInfo]>.ObservationID
    ) async {
        self.appsObservationRegistry.removeObservation(observationID)
        let task = self.appsObservationTasks.removeValue(forKey: observationID)
        await task?.value
    }

    nonisolated private static func makeMonitorTask(
        agent: WendyAgent,
        mainServerTask: Task<Void, Error>,
        otelServerTask: Task<Void, Error>,
        bonjourTask: Task<Void, Error>,
        runIdentifier: UInt64
    ) -> Task<Void, Never> {
        Task.detached {
            await agent.monitorRuntimeTasks(
                mainServerTask: mainServerTask,
                otelServerTask: otelServerTask,
                bonjourTask: bonjourTask,
                runIdentifier: runIdentifier
            )
        }
    }

    nonisolated private static func makeServeTask(server: PosixGRPCServer) -> Task<Void, Error> {
        Task {
            try await server.serve()
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
