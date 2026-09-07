import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOSSL
import OpenTelemetryGRPC
import WendyAgentGRPC
import X509

public actor WendyAgent {
    private typealias PosixGRPCServer = GRPCServer<HTTP2ServerTransport.Posix>

    /// The Wendy Agent version from the main bundle Info.plist.
    public nonisolated static let version: String = {
        guard
            let version = Bundle.main.object(forInfoDictionaryKey: "WLWendyAgentVersion")
                as? String,
            !version.isEmpty
        else {
            fatalError("Missing WLWendyAgentVersion in the main bundle Info.plist")
        }

        return version
    }()

    public let configuration: WendyAgentConfiguration
    public private(set) var status: WendyAgentStatus = .idle
    public private(set) var apps: [WendyAppInfo] = []

    /// Snapshotted before any RPC service starts so post-update verification
    /// describes the executable this process actually mapped.
    private let runningExecutableSHA256: String

    public init(configuration: WendyAgentConfiguration = .init()) {
        self.configuration = configuration
        self.runningExecutableSHA256 = AgentExecutableDigest.sha256(at: Bundle.main.executableURL)
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
        self.telemetryBroadcaster = broadcaster

        do {
            let backend = await self.makeLinuxBackend()

            try await self.startMainServer(
                linuxBackend: backend,
                broadcaster: broadcaster
            )
            try await self.startOTelServer(broadcaster: broadcaster)
            try await self.startBonjour()

            // Registry listeners come after startMainServer: the push listener
            // reads the provisioned identity from provisioningService, which
            // startMainServer creates. Best-effort — a failed registry bind
            // must not brick native-app management on the device.
            if backend != nil {
                self.startPullRegistryIfNeeded()
                await self.startPushRegistry()
            }

            self.runIdentifier &+= 1
            self.handlingUnexpectedRuntimeExit = false
            self.startMonitorTask(runIdentifier: self.runIdentifier)
            // Cold start: apps recorded as running are leftovers from a previous
            // agent process, so reconcile them.
            self.startAppSupervisor(reconcile: true)

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

        // Order matters: mark the service as stopping so no further supervisor
        // tick does any work, then cancel the supervisor and wait for a tick
        // that is already in flight, and only then stop the apps. Otherwise a
        // restart could race the shutdown and leave an orphaned child behind.
        await self.containerService?.beginStopping()
        await self.stopAppSupervisor()
        await self.containerService?.stopAllApps()

        await self.stopBonjour()
        await self.stopOTelServer()
        await self.stopMainServer()

        await self.stopRegistryListeners()

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

    /// Registers the app's clean-quit path, used to terminate the process once
    /// a self-update has been installed. Call before `start()`: the handler is
    /// captured when the main server's services are built.
    public func setAgentTerminationHandler(_ handler: @escaping @Sendable () async -> Void) {
        self.agentTerminationHandler = handler
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

    /// Held here, not on `AgentService`, so a single in-flight update stays
    /// serialized across the service rebuilds that provisioning transitions
    /// perform (`switchMainServer()`).
    private let agentUpdateLock = AgentUpdateLock()
    /// The app's clean-quit path, invoked after an agent update is committed.
    /// `nil` (e.g. in tests) falls back to `exit(0)` inside `AgentRelauncher`.
    private var agentTerminationHandler: (@Sendable () async -> Void)?

    private var mainServer: PosixGRPCServer?
    private var mainServerTask: Task<Void, any Error>?
    private var containerService: ContainerService?
    /// The embedded OCI registry's loopback pull listener
    /// (`127.0.0.1:5556`, plain HTTP, read-only routes) that the on-device
    /// Linux container backends pull from. Started once and kept for the
    /// process lifetime — it never changes shape, so provisioning transitions
    /// leave it alone. Cancelled (and awaited, so the port is released) only
    /// on a full agent stop or a failed startup.
    private var pullRegistryTask: Task<Void, Never>?
    /// The registry's wildcard push listener (port 5555, all interfaces) that
    /// the CLI pushes images to: plain HTTP while unprovisioned, TLS with
    /// client-certificate verification once provisioned. Unlike the pull
    /// listener it IS restarted on provisioning transitions (via
    /// `switchMainServer()` → `startPushRegistry()`) so its scheme tracks the
    /// device's provisioning state, mirroring the Linux agent's registry.
    private var pushRegistryTask: Task<Void, Never>?

    /// The cloud tunnel-broker presence/relay loop, tied to the mTLS main
    /// server's lifetime: started in `startMainServer` when the server comes up
    /// in mTLS mode (provisioned), cancelled in `stopMainServer`. This makes the
    /// device reachable through the cloud relay while provisioned; a plaintext
    /// (unprovisioned) server never starts it.
    private var tunnelBrokerTask: Task<Void, Never>?

    /// The provisioning service registered on the currently-running main server.
    /// Rebuilt (with callbacks re-wired) every time the main server starts so the
    /// next provision/unprovision transition still fires.
    private var provisioningService: ProvisioningService?
    /// Whether the main server is currently running in mTLS mode (provisioned) as
    /// opposed to plaintext (unprovisioned).
    private var mainServerIsMTLS = false
    /// Guards against overlapping plaintext<->mTLS transitions.
    private var switchingMainServer = false
    /// How long `switchMainServer` waits before tearing the old server down, to
    /// let the provisioning RPC that triggered the switch flush its response to
    /// the client first (mirrors the Go agent's delayed restart).
    private static let serverSwitchFlushDelay: Duration = .milliseconds(500)
    /// The telemetry broadcaster shared by the main server's `TelemetryService`
    /// and the OTel receivers; retained so a mid-flight main-server switch reuses
    /// it and telemetry continuity is preserved.
    private var telemetryBroadcaster: TelemetryBroadcaster?

    private var otelServer: PosixGRPCServer?
    private var otelServerTask: Task<Void, any Error>?

    private var bonjourRegistration: BonjourRegistration?
    private var bonjourTask: Task<Void, any Error>?

    private var monitorTask: Task<Void, Never>?
    /// Restart supervision for deployed apps: one reconcile pass that brings
    /// apps back after an agent restart, then a periodic tick that restarts
    /// crashed ones per their policy. Held here — next to the other runtime
    /// task handles — so every teardown path cancels it, including the
    /// provisioning-driven server switch, which builds a fresh
    /// `ContainerService` the old task must not keep supervising.
    private var appSupervisorTask: Task<Void, Never>?
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
    /// Probes for an available Linux container runtime, preferring Apple's
    /// `container` over Docker, and returns the concrete backend to use (or
    /// `nil` if neither is installed/running).
    private func makeLinuxBackend() async -> (any LinuxContainerBackend)? {
        let containerAvailable = await ContainerCLI().checkAvailable()
        let dockerAvailable = await DockerCLI().checkAvailable()
        switch LinuxRuntimeSelector.choose(
            containerAvailable: containerAvailable,
            dockerAvailable: dockerAvailable
        ) {
        case .appleContainer:
            self.logger.info("Linux container runtime: Apple container")
            // Warm up the container system services in the background so the
            // first deploy doesn't pay the boot latency. Detached and
            // best-effort — never block agent startup on it, and the backend
            // also starts them before each pull, so a failure here is harmless.
            let warmupLogger = self.logger
            Task.detached {
                do {
                    try await ContainerCLI().systemStart()
                } catch {
                    warmupLogger.warning(
                        "container system start failed at startup; will retry on first pull",
                        metadata: ["error": "\(error)"]
                    )
                }
            }
            return ContainerCLIBackend()
        case .docker:
            self.logger.info("Linux container runtime: Docker")
            return DockerContainerBackend()
        case nil:
            self.logger.info("No Linux container runtime available; native macOS apps only")
            return nil
        }
    }

    private func startMainServer(
        linuxBackend: (any LinuxContainerBackend)?,
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
            linuxBackend: linuxBackend,
            otelPort: self.configuration.otelPort,
            onAppsChanged: { [weak self] apps in
                await self?.updateApps(apps)
            }
        )
        self.containerService = containerService
        await containerService.publishCurrentApps()

        // Build the provisioning service, hold it, and wire the transition
        // callbacks. The callbacks are invoked from inside the provisioning RPC
        // handler *before* it returns; they MUST NOT await the server switch or
        // graceful shutdown would deadlock on the in-flight RPC. Each callback
        // therefore kicks off the switch in a detached task and returns at once.
        let provisioningService = ProvisioningService(configPath: stateDirectory)
        self.provisioningService = provisioningService
        await provisioningService.setCallbacks(
            onProvisioned: { [weak self] _ in
                // Fire-and-forget: the switch (and its graceful shutdown of the
                // current server) MUST NOT be awaited here — this callback runs
                // inside the in-flight provisioning RPC, which that shutdown
                // drains, so awaiting it would deadlock. Detach and return.
                Task { await self?.handleProvisioned() }
            },
            onUnprovisioned: { [weak self] in
                Task { await self?.handleUnprovisioned() }
            }
        )
        let info = await provisioningService.provisioningInfo()

        let services: [any RegistrableRPCService] = [
            AgentService(
                binarySHA256: self.runningExecutableSHA256,
                updateLock: self.agentUpdateLock,
                updateDependencies: .init(
                    relauncher: AgentRelauncher(terminate: self.agentTerminationHandler)
                )
            ),
            containerService,
            AudioService(),
            provisioningService,
            TelemetryService(broadcaster: broadcaster),
            FileSyncService(appsBase: appsBase),
        ]

        // When enrolled, run mTLS on `port + 1`; otherwise plaintext on `port`.
        let certs = info.enrolled ? await provisioningService.provisioningCerts() : nil
        let (server, isMTLS) = try self.makeMainServer(services: services, certs: certs)
        self.mainServerIsMTLS = isMTLS
        let boundPort = isMTLS ? self.configuration.port + 1 : self.configuration.port

        let task = Self.makeServeTask(server: server)

        do {
            if let address = try await server.listeningAddress {
                self.logger.info(
                    "Main Wendy Agent gRPC server listening",
                    metadata: ["grpc_address": "\(address)", "mtls": "\(isMTLS)"]
                )
            } else {
                self.logger.info(
                    "Main Wendy Agent gRPC server listening",
                    metadata: ["mtls": "\(isMTLS)"]
                )
            }

            self.mainServer = server
            self.mainServerTask = task

            // Once the mTLS server is listening, dial out to the cloud tunnel
            // broker so the device shows online and is remotely reachable.
            if isMTLS, let certs {
                self.startTunnelBroker(info: info, certs: certs)
            }
        } catch {
            server.beginGracefulShutdown()
            throw await Self.startupError(
                serviceName: "Wendy Agent gRPC",
                port: boundPort,
                listeningAddressError: error,
                serveTask: task
            )
        }
    }

    /// Starts the tunnel-broker loop for a provisioned (mTLS) main server. The
    /// broker URL comes from the provisioning `cloudHost` (with a
    /// `WENDY_BROKER_URL` override); org/asset from the provisioning info; the
    /// trust anchor from the device CA chain; the relay target is the local mTLS
    /// port. A missing chain skips the broker rather than dialing without a trust
    /// anchor (mirrors the Go agent).
    private func startTunnelBroker(
        info: ProvisioningService.ProvisioningInfo,
        certs: ProvisioningService.ProvisioningCerts
    ) {
        guard !certs.chainPEM.isEmpty else {
            self.logger.warning(
                "CA chain unavailable; not starting tunnel broker (re-provision if this persists)"
            )
            return
        }
        let config = TunnelBrokerClient.Config(
            brokerURL: TunnelBrokerClient.brokerURL(
                cloudHost: info.cloudHost,
                override: ProcessInfo.processInfo.environment["WENDY_BROKER_URL"]
            ),
            orgID: info.orgID,
            assetID: info.assetID,
            certificatePEM: certs.certPEM,
            keyBacking: certs.keyBacking,
            seKey: certs.seKey,
            chainPEM: certs.chainPEM,
            mtlsPort: self.configuration.port + 1
        )
        let client = TunnelBrokerClient.live
        let logger = self.logger
        self.tunnelBrokerTask = Task {
            await client.runForever(config: config, logger: logger)
        }
    }

    private func stopMainServer() async {
        // Stop dialing the broker before tearing the server down.
        self.tunnelBrokerTask?.cancel()
        await self.tunnelBrokerTask?.value
        self.tunnelBrokerTask = nil

        self.mainServer?.beginGracefulShutdown()
        _ = try? await self.mainServerTask?.value
        self.mainServer = nil
        self.mainServerTask = nil
    }

    /// Builds the main gRPC server. When `certs` is non-nil the server runs mTLS
    /// on `port + 1`, requiring and verifying client certificates against the
    /// device CA chain and enforcing org-equality; otherwise it runs plaintext on
    /// `port`. Returns the server and whether it is mTLS.
    private func makeMainServer(
        services: [any RegistrableRPCService],
        certs: ProvisioningService.ProvisioningCerts?
    ) throws -> (PosixGRPCServer, Bool) {
        let security: HTTP2ServerTransport.Posix.TransportSecurity
        let port: Int
        if let certs {
            security = try self.mTLSSecurity(certs: certs)
            port = self.configuration.port + 1
        } else {
            security = .plaintext
            port = self.configuration.port
        }

        let server = PosixGRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: {
                    switch self.configuration.host {
                    case "::", "::1":
                        .ipv6(host: self.configuration.host, port: port)
                    case "localhost":
                        .ipv4(host: "127.0.0.1", port: port)
                    default:
                        .ipv4(host: self.configuration.host, port: port)
                    }
                }(),
                transportSecurity: security,
                config: .defaults {
                    $0.http2.maxFrameSize = 256 * 1024
                    $0.http2.targetWindowSize = 8 * 1024 * 1024
                    $0.rpc.maxRequestPayloadSize = 16 * 1024 * 1024
                }
            ),
            services: services
        )
        return (server, certs != nil)
    }

    /// Constructs the mTLS transport security for the main server.
    ///
    /// `clientCertificateVerification` is `.noHostnameVerification` (rather than
    /// `.noVerification`) for two reasons: a client certificate is required, and
    /// NIOSSL only invokes `customVerificationCallback` when verification is not
    /// disabled. The custom callback fully REPLACES BoringSSL's chain validation,
    /// so `ClientCertAuthorizer` performs the complete verification itself: it
    /// builds a verified path to the device's own CA trust roots AND applies the
    /// org-enforcement policy (`WENDY_MTLS_ORG_ENFORCEMENT`, default grace). It
    /// fails closed. The device's org is derived once, here, from the device's
    /// own leaf certificate — never by calling back into the provisioning actor
    /// from the event loop.
    private func mTLSSecurity(
        certs: ProvisioningService.ProvisioningCerts
    ) throws -> HTTP2ServerTransport.Posix.TransportSecurity {
        let leaf = TLSConfig.CertificateSource.bytes(Array(certs.certPEM.utf8), format: .pem)
        let chain = TLSConfig.CertificateSource.bytes(Array(certs.chainPEM.utf8), format: .pem)
        let key = try tlsPrivateKeySource(certs.keyBacking, seKey: certs.seKey)

        let trustRootsPEM = certs.chainPEM
        let deviceOrg = ClientCertAuthorizer.organizationID(fromLeafPEM: certs.certPEM)
        if deviceOrg == nil {
            self.logger.error(
                "Could not determine device organization from its own certificate; mTLS will reject all clients (fail closed). Re-provision the device to recover."
            )
        }

        // Org-enforcement mode (WENDY_MTLS_ORG_ENFORCEMENT: off|grace|strict).
        // Defaults to grace so today's CLI user certs — which carry no org claim
        // — can connect while cert rotation to org-bearing URNs completes.
        let rawOrgEnforcement = ProcessInfo.processInfo.environment["WENDY_MTLS_ORG_ENFORCEMENT"]
        let (orgMode, recognized) = ClientCertAuthorizer.OrgEnforcementMode.parse(rawOrgEnforcement)
        if !recognized {
            self.logger.warning(
                "Unrecognized WENDY_MTLS_ORG_ENFORCEMENT value; defaulting to grace",
                metadata: [
                    "value": "\(rawOrgEnforcement ?? "")"
                ]
            )
        }
        self.logger.info(
            "mTLS client org enforcement",
            metadata: ["mode": "\(orgMode.name)"]
        )

        var tls = HTTP2ServerTransport.Posix.TransportSecurity.TLS(
            certificateChain: [leaf, chain],
            privateKey: key,
            clientCertificateVerification: .noHostnameVerification,
            trustRoots: .certificates([chain]),
            requireALPN: false
        )
        tls.customVerificationCallback = { peerCertificates, promise in
            let ders = peerCertificates.compactMap { try? $0.toDERBytes() }
            Task {
                let authorized = await ClientCertAuthorizer.isAuthorized(
                    peerCertificatesDER: ders,
                    trustRootsPEM: trustRootsPEM,
                    deviceOrg: deviceOrg,
                    mode: orgMode
                )
                promise.succeed(authorized ? .certificateVerified(.init(nil)) : .failed)
            }
        }
        return .tls(tls)
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
            throw await Self.startupError(
                serviceName: "local OpenTelemetry gRPC",
                port: self.configuration.otelPort,
                listeningAddressError: error,
                serveTask: task
            )
        }
    }

    private func stopOTelServer() async {
        self.otelServer?.beginGracefulShutdown()
        _ = try? await self.otelServerTask?.value
        self.otelServer = nil
        self.otelServerTask = nil
    }

    private func startBonjour() async throws {
        let info = await self.provisioningService?.provisioningInfo()
        let enrolled = info?.enrolled ?? false
        let advertiser = BonjourAdvertiser(
            port: enrolled ? self.configuration.port + 1 : self.configuration.port,
            displayName: ProcessInfo.processInfo.hostName,
            deviceID: ProcessInfo.processInfo.hostName,
            tls: enrolled,
            assetID: enrolled ? info?.assetID : nil
        )

        let runtime = try await advertiser.start()
        self.logger.info(
            "Bonjour advertisement registered",
            metadata: ["tls": "\(enrolled)"]
        )

        self.bonjourRegistration = runtime.registration
        self.bonjourTask = runtime.task
    }

    private func stopBonjour() async {
        await self.bonjourRegistration?.shutdown()
        _ = try? await self.bonjourTask?.value
        self.bonjourRegistration = nil
        self.bonjourTask = nil
    }

    /// Called (via a detached task) after the device is provisioned. Switches the
    /// main server from plaintext to mTLS and re-advertises Bonjour.
    private func handleProvisioned() async {
        guard case .running = self.status, !self.mainServerIsMTLS else { return }
        self.logger.info("Device provisioned — switching main server to mTLS")
        await self.switchMainServer()
    }

    /// Called (via a detached task) after the device is unprovisioned. Switches
    /// the main server from mTLS back to plaintext and re-advertises Bonjour.
    private func handleUnprovisioned() async {
        guard case .running = self.status, self.mainServerIsMTLS else { return }
        self.logger.info("Device unprovisioned — switching main server to plaintext")
        await self.switchMainServer()
    }

    /// Rebuilds and restarts the main gRPC server (and Bonjour) in whatever mode
    /// the now-updated provisioning state dictates.
    ///
    /// Concurrency: this only ever runs from a detached task spawned by the
    /// provisioning callbacks, never synchronously from the provisioning RPC, so
    /// the graceful shutdown of the old server here cannot deadlock on the RPC
    /// that triggered it. A short delay first lets that RPC's response flush
    /// (mirroring the Go agent's delayed restart). The runtime monitor is stopped
    /// before teardown so the intentional stop isn't misread as a crash, then
    /// resumed with the fresh task handles.
    private func switchMainServer() async {
        guard case .running = self.status, !self.switchingMainServer else { return }
        self.switchingMainServer = true
        defer { self.switchingMainServer = false }

        // A cancelled sleep just proceeds to the status re-check below, which is
        // the intended behavior during shutdown.
        try? await Task.sleep(for: Self.serverSwitchFlushDelay)
        guard case .running = self.status else { return }

        self.stopMonitorTask()
        // The switch builds a fresh ContainerService, so the supervisor has to
        // be torn down with the old one and restarted against the new one.
        await self.stopAppSupervisor()
        await self.stopBonjour()
        await self.stopMainServer()

        let backend = await self.makeLinuxBackend()
        let broadcaster = self.telemetryBroadcaster ?? TelemetryBroadcaster()

        do {
            try await self.startMainServer(
                linuxBackend: backend,
                broadcaster: broadcaster
            )
            try await self.startBonjour()
        } catch {
            self.logger.error(
                "Failed to switch main server after provisioning change",
                metadata: ["error": "\(Self.errorMessage(for: error))"]
            )
            await self.rollbackStartup()
            self.clearRuntimeState()
            self.updateStatus(.failed(Self.errorMessage(for: error)))
            return
        }

        // Restart the push listener so its scheme (plain HTTP vs mTLS) tracks
        // the new provisioning state; the loopback pull listener never changes
        // shape. Runs only from this detached-task context, never inside the
        // provisioning RPC, so awaiting the old listener's shutdown here cannot
        // deadlock the RPC that triggered the switch.
        if backend != nil {
            self.startPullRegistryIfNeeded()
            await self.startPushRegistry()
        }

        self.runIdentifier &+= 1
        self.handlingUnexpectedRuntimeExit = false
        self.startMonitorTask(runIdentifier: self.runIdentifier)
        // Warm restart: the apps this switch's new ContainerService just loaded
        // as stopped are still running under the process handles the previous
        // one held, so supervise from here on but do NOT reconcile.
        self.startAppSupervisor(reconcile: false)

        self.logger.info(
            "Main server switched",
            metadata: ["mtls": "\(self.mainServerIsMTLS)"]
        )
    }

    /// Starts the loopback pull listener (`127.0.0.1:5556`, plain HTTP,
    /// read-only routes) once per process. Best-effort: a bind failure is
    /// logged with a port-in-use hint but never fails agent startup.
    private func startPullRegistryIfNeeded() {
        guard self.pullRegistryTask == nil else { return }
        let registry = AgentImageRegistry(
            store: BlobStore(root: WendyAgentPaths.stateDirectory),
            configuration: .init(
                host: LocalRegistryRef.pullHost,
                port: LocalRegistryRef.pullPort,
                tls: nil,
                routes: .pullOnly,
                label: "pull"
            )
        )
        let logger = self.logger
        self.pullRegistryTask = Task {
            do {
                try await registry.run()
            } catch {
                Self.logRegistryListenerStopped(
                    logger,
                    listener: "pull",
                    port: LocalRegistryRef.pullPort,
                    error: error
                )
            }
        }
    }

    /// (Re)starts the wildcard push listener (port 5555, all interfaces):
    /// plain HTTP while unprovisioned, TLS with client-certificate
    /// verification once provisioned. Always stop-then-start so a provisioning
    /// transition swaps the scheme (mirroring the Linux agent's registry
    /// restart). Fail closed: if the device is provisioned but its TLS
    /// material is unusable, `run()` throws and the listener stays DOWN — it
    /// never falls back to plain HTTP on a non-loopback interface. Best-effort
    /// beyond that: a bind failure (e.g. a stale `wendy-registry` Docker
    /// container squatting on 5555) must not brick native-app management or
    /// provisioning, so it is logged, not fatal.
    private func startPushRegistry() async {
        self.pushRegistryTask?.cancel()
        await self.pushRegistryTask?.value
        self.pushRegistryTask = nil

        var tls: RegistryTLS.Configuration?
        if let certs = await self.provisioningService?.provisioningCerts() {
            tls = RegistryTLS.makeConfiguration(certs: certs, logger: self.logger)
        }
        let registry = AgentImageRegistry(
            store: BlobStore(root: WendyAgentPaths.stateDirectory),
            configuration: .init(
                host: "::",
                port: LocalRegistryRef.pushPort,
                tls: tls,
                routes: .pushAndPull,
                label: "push"
            )
        )
        let logger = self.logger
        self.pushRegistryTask = Task {
            do {
                try await registry.run()
            } catch {
                Self.logRegistryListenerStopped(
                    logger,
                    listener: "push",
                    port: LocalRegistryRef.pushPort,
                    error: error
                )
            }
        }
    }

    /// Cancels and awaits both registry listeners so their ports are released.
    private func stopRegistryListeners() async {
        self.pushRegistryTask?.cancel()
        self.pullRegistryTask?.cancel()
        await self.pushRegistryTask?.value
        await self.pullRegistryTask?.value
        self.pushRegistryTask = nil
        self.pullRegistryTask = nil
    }

    private static func logRegistryListenerStopped(
        _ logger: Logger,
        listener: String,
        port: Int,
        error: any Error
    ) {
        if error is CancellationError { return }
        var metadata: Logger.Metadata = [
            "listener": "\(listener)",
            "port": "\(port)",
            "error": "\(error)",
        ]
        if Self.isAddressAlreadyInUse(error) {
            metadata["hint"] =
                "port \(port) is already in use — check for a stale `wendy-registry` Docker container or another registry on this Mac"
        }
        logger.error("Agent image registry listener stopped", metadata: metadata)
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

    /// Starts crash supervision against the current `ContainerService`, with an
    /// optional one-shot reconcile pass first. Safe to call repeatedly: any
    /// previous supervisor is cancelled first.
    ///
    /// `reconcile` must be true only on a cold start. Reconcile assumes nothing
    /// else is running the apps: it terminates native processes recorded in
    /// `info.json` as survivors of a disorderly exit. On a warm restart — a
    /// provisioning transition, which rebuilds the `ContainerService` over the
    /// same state directory while the previous one's app processes are still
    /// very much alive — those pids are not survivors, and reconciling would
    /// SIGTERM every running native app: a downtime blip for `unless-stopped`
    /// apps, and permanent death for `--no-restart` ones.
    private func startAppSupervisor(reconcile: Bool) {
        guard let containerService = self.containerService else { return }
        self.cancelAppSupervisorTask()
        self.appSupervisorTask = Self.makeAppSupervisorTask(
            containerService: containerService,
            reconcile: reconcile
        )
    }

    @discardableResult
    private func cancelAppSupervisorTask() -> Task<Void, Never>? {
        let task = self.appSupervisorTask
        self.appSupervisorTask = nil
        task?.cancel()
        return task
    }

    /// Cancels supervision and waits for an in-flight tick to finish, so a
    /// restart cannot race whatever shutdown follows.
    private func stopAppSupervisor() async {
        await self.cancelAppSupervisorTask()?.value
    }

    /// Internal rather than private so the cold/warm-start distinction can be
    /// exercised directly, without standing up a whole agent.
    nonisolated static func makeAppSupervisorTask(
        containerService: ContainerService,
        reconcile: Bool
    ) -> Task<Void, Never> {
        let interval = containerService.supervisorInterval
        return Task.detached {
            // Reconcile runs inside this task rather than being awaited by
            // `start()`: bringing apps back takes seconds (a Linux image pull,
            // far longer), and the gRPC server has to be answering RPCs the
            // moment it is listening.
            if reconcile {
                await containerService.reconcileApps()
            }

            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: interval)
                } catch {
                    return  // Cancelled.
                }
                await containerService.superviseApps()
            }
        }
    }

    private func rollbackStartup() async {
        await self.stopBonjour()
        await self.stopOTelServer()
        await self.stopMainServer()

        await self.stopRegistryListeners()
    }

    private func clearRuntimeState() {
        self.mainServer = nil
        self.mainServerTask = nil
        self.tunnelBrokerTask = nil
        self.containerService = nil
        self.provisioningService = nil
        self.mainServerIsMTLS = false
        self.telemetryBroadcaster = nil
        self.otelServer = nil
        self.otelServerTask = nil
        self.bonjourRegistration = nil
        self.bonjourTask = nil
        self.stopMonitorTask()
        // Cancelled (not awaited) — this is the synchronous teardown path, and
        // the supervisor holds the last reference to the ContainerService being
        // dropped, so leaving it running would leak both.
        self.cancelAppSupervisorTask()
        self.handlingUnexpectedRuntimeExit = false
    }

    private func monitorRuntimeTasks(
        mainServerTask: Task<Void, any Error>,
        otelServerTask: Task<Void, any Error>,
        bonjourTask: Task<Void, any Error>,
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
        _ task: Task<Void, any Error>,
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

        // Order matters, mirroring `stop()` (WendyAgent.swift:107-113): mark the
        // service as stopping so no further supervisor tick does any work, then
        // cancel the supervisor and wait for a tick that is already in flight,
        // before tearing anything else down. Without this a tick already inside
        // `superviseApps` could keep launching apps into a `ContainerService`
        // that is about to be dropped, and a subsequent `start()` in the same
        // process would race its cold reconcile against those launches.
        await self.containerService?.beginStopping()
        await self.stopAppSupervisor()

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

        let task = Task {
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

        let task = Task {
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
        mainServerTask: Task<Void, any Error>,
        otelServerTask: Task<Void, any Error>,
        bonjourTask: Task<Void, any Error>,
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

    nonisolated private static func makeServeTask(server: PosixGRPCServer) -> Task<Void, any Error>
    {
        Task {
            try await server.serve()
        }
    }

    private static func startupError(
        serviceName: String,
        port: Int,
        listeningAddressError: any Error,
        serveTask: Task<Void, any Error>
    ) async -> any Error {
        do {
            try await serveTask.value
        } catch {
            return Self.startupError(
                serviceName: serviceName,
                port: port,
                underlyingError: error
            )
        }

        return Self.startupError(
            serviceName: serviceName,
            port: port,
            underlyingError: listeningAddressError
        )
    }

    private static func startupError(
        serviceName: String,
        port: Int,
        underlyingError: any Error
    ) -> any Error {
        if Self.isAddressAlreadyInUse(underlyingError) {
            return WendyAgentError.portInUse(serviceName: serviceName, port: port)
        }

        return underlyingError
    }

    private static func isAddressAlreadyInUse(_ error: any Error) -> Bool {
        if let runtimeError = error as? RuntimeError,
            let cause = runtimeError.cause,
            Self.isAddressAlreadyInUse(cause)
        {
            return true
        }

        let description = String(describing: error).lowercased()
        return description.contains("address already in use")
            || description.contains("errno: 48")
            || description.contains("errno: 98")
    }

    private static func errorMessage(for error: any Error) -> String {
        if let localizedError = error as? any LocalizedError,
            let description = localizedError.errorDescription,
            !description.isEmpty
        {
            return description
        }

        let description = String(describing: error)
        return description.isEmpty ? "WendyAgent failed to start." : description
    }
}
