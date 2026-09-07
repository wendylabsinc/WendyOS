import CryptoKit
import Foundation
import GRPCCore
import Logging
import OpenTelemetryGRPC
import SwiftProtobuf
import WendyAgentGRPC

#if os(macOS)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#endif

/// Looks up the executable path of a live pid, returning `nil` when the pid is
/// not alive (or cannot be inspected). Injected into `ContainerService` so the
/// native-survivor logic is testable without spawning real processes.
typealias PIDExecutablePathLookup = @Sendable (Int32) -> String?

/// Sends a signal to a pid. Injected alongside `PIDExecutablePathLookup` so
/// tests can assert exactly which pids were signalled — and, more importantly,
/// which were not.
typealias PIDSignalSender = @Sendable (Int32, Int32) -> Void

/// Client-facing view of an app-owned stdout/stderr drain. Finishing this
/// stream stops delivery to a disconnected RPC without stopping pipe reads or
/// telemetry broadcast by the app-lifetime task.
private final class ContainerTaskLifetimeOutput: Sendable {
    let stream: AsyncStream<Wendy_Agent_Services_V1_RunContainerLayersResponse>
    private let continuation:
        AsyncStream<Wendy_Agent_Services_V1_RunContainerLayersResponse>.Continuation

    init() {
        (self.stream, self.continuation) = AsyncStream.makeStream(
            of: Wendy_Agent_Services_V1_RunContainerLayersResponse.self,
            bufferingPolicy: .bufferingNewest(256)
        )
    }

    func yield(data: Data, fromStdout: Bool) {
        var response = Wendy_Agent_Services_V1_RunContainerLayersResponse()
        var consoleOutput = Wendy_Agent_Services_V1_RunContainerLayersResponse.ConsoleOutput()
        consoleOutput.data = data
        response.responseType =
            fromStdout ? .stdoutOutput(consoleOutput) : .stderrOutput(consoleOutput)
        self.continuation.yield(response)
    }

    func finish() {
        self.continuation.finish()
    }
}

actor ContainerService: Wendy_Agent_Services_V1_WendyContainerService.ServiceProtocol {
    private let appsBase: URL
    private let blobsDirectory: String
    private let broadcaster: TelemetryBroadcaster
    private let infoFileURL: URL
    private let otelPort: Int
    private let onAppsChanged: @Sendable ([WendyAppInfo]) async -> Void
    private typealias NativeLaunchInfo = WendyApp.NativeMetadata

    private let executablePath: String
    private let logger = Logger(label: "sh.wendy.agent.container")
    private let nativeStopTimeout: Duration = .seconds(5)
    private var appsByID: [String: WendyApp] = [:]
    private var isStopping = false
    private let sandboxProfilePath: String?

    /// Linux container runtime (Apple `container` or Docker). Nil when neither is present.
    private let linuxBackend: (any LinuxContainerBackend)?
    private let linuxUnavailableMessage: String

    /// How often the crash supervisor ticks. Mirrors the Linux agent's 15 s
    /// monitor interval (`go/cmd/wendy-agent/main.go:229`). `nonisolated` so
    /// the task driving the tick can read it without hopping onto the actor.
    nonisolated let supervisorInterval: Duration
    /// Minimum gap between two automatic restarts of the same app, mirroring
    /// the Linux monitor's 10 s floor (`planRestarts`).
    private let restartFloorSeconds: TimeInterval
    private let pidExecutablePath: PIDExecutablePathLookup
    private let pidBirthTime: PIDBirthTimeLookup
    private let sendSignal: PIDSignalSender

    init(
        broadcaster: TelemetryBroadcaster,
        executablePath: String,
        sandboxProfilePath: String? = nil,
        stateDirectory: URL? = nil,
        appsBase: URL? = nil,
        linuxBackend: (any LinuxContainerBackend)? = nil,
        linuxUnavailableMessage: String =
            "No Linux container runtime found. Install Apple's `container` (recommended) or Docker on the Mac agent.",
        otelPort: Int = 4317,
        supervisorInterval: Duration = .seconds(15),
        restartFloor: Duration = .seconds(10),
        pidExecutablePath: @escaping PIDExecutablePathLookup = {
            ContainerService.executablePath(forPID: $0)
        },
        pidBirthTime: @escaping PIDBirthTimeLookup = {
            NativeProcessConfiguration.birthTime(forPID: $0)
        },
        sendSignal: @escaping PIDSignalSender = { pid, signal in _ = Darwin.kill(pid, signal) },
        onAppsChanged: @escaping @Sendable ([WendyAppInfo]) async -> Void = { _ in }
    ) {
        self.broadcaster = broadcaster
        self.onAppsChanged = onAppsChanged
        self.executablePath = executablePath
        self.sandboxProfilePath = sandboxProfilePath
        self.linuxBackend = linuxBackend
        self.linuxUnavailableMessage = linuxUnavailableMessage
        self.otelPort = otelPort
        self.supervisorInterval = supervisorInterval
        self.restartFloorSeconds = Self.seconds(restartFloor)
        self.pidExecutablePath = pidExecutablePath
        self.pidBirthTime = pidBirthTime
        self.sendSignal = sendSignal

        let defaultStateDirectory = WendyAgentPaths.stateDirectory
        let resolvedStateDirectory = stateDirectory ?? appsBase ?? defaultStateDirectory

        self.appsBase = appsBase ?? resolvedStateDirectory.appendingPathComponent("apps")
        self.blobsDirectory = resolvedStateDirectory.appendingPathComponent("blobs").path
        self.infoFileURL = resolvedStateDirectory.appendingPathComponent("info.json")

        // Ensure directories exist.
        try? FileManager.default.createDirectory(
            at: resolvedStateDirectory,
            withIntermediateDirectories: true
        )
        try? FileManager.default.createDirectory(
            at: self.appsBase,
            withIntermediateDirectories: true
        )
        try? FileManager.default.createDirectory(
            atPath: "\(blobsDirectory)/sha256",
            withIntermediateDirectories: true
        )

        self.appsByID = Self.loadApps(from: self.infoFileURL, logger: self.logger)
    }

    private func currentAppInfos() -> [WendyAppInfo] {
        self.appsByID.values.map(\.info).sorted { $0.id < $1.id }
    }

    func currentAppInfosForTesting() -> [WendyAppInfo] {
        self.currentAppInfos()
    }

    func infoFileURLForTesting() -> URL {
        self.infoFileURL
    }

    func publishCurrentApps() async {
        await self.publishApps()
    }

    private func publishApps() async {
        await self.onAppsChanged(self.currentAppInfos())
    }

    nonisolated private static func loadApps(
        from infoFileURL: URL,
        logger: Logger
    ) -> [String: WendyApp] {
        guard FileManager.default.fileExists(atPath: infoFileURL.path) else { return [:] }

        do {
            let data = try Data(contentsOf: infoFileURL)
            let persistedApps = try JSONDecoder().decode([WendyApp].self, from: data)
            return Dictionary(
                uniqueKeysWithValues: persistedApps.map { app in
                    var restoredApp = app
                    restoredApp.info = WendyAppInfo(
                        id: app.info.id,
                        kind: app.info.kind,
                        status: .stopped,
                        pid: nil
                    )
                    restoredApp.process = nil
                    restoredApp.launchToken = nil
                    // The pid is scrubbed from `info` (this process owns no
                    // handle on it) but kept aside: if the previous agent
                    // process exited disorderly, that pid may still be running
                    // this app, and reconcile has to deal with the survivor.
                    restoredApp.persistedPID = app.info.pid
                    return (restoredApp.info.id, restoredApp)
                }
            )
        } catch {
            logger.warning(
                "Failed to load persisted apps",
                metadata: [
                    "path": "\(infoFileURL.path)",
                    "error": "\(String(describing: error))",
                ]
            )
            return [:]
        }
    }

    private func saveApps() throws {
        let persistedApps = self.appsByID.values.sorted { $0.info.id < $1.info.id }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(persistedApps)
        try FileManager.default.createDirectory(
            at: self.infoFileURL.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try data.write(to: self.infoFileURL, options: .atomic)
    }

    func appInfo(forAppID id: String) -> WendyAppInfo? {
        self.appsByID[id]?.info
    }

    func launchToken(forAppID id: String) -> UUID? {
        self.appsByID[id]?.launchToken
    }

    func lastExitCode(forAppID id: String) -> Int32? {
        self.appsByID[id]?.lastExitCode
    }

    func failureCount(forAppID id: String) -> Int? {
        self.appsByID[id]?.failureCount
    }

    func persistedPID(forAppID id: String) -> Int32? {
        self.appsByID[id]?.persistedPID
    }

    func beginStopping() {
        self.isStopping = true
    }

    func stopApp(id: String) async {
        do {
            _ = try await self.stopTrackedAppIfRunning(id: id)
        } catch {
            self.logger.error(
                "Failed to stop app",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }
    }

    func stopAllApps() async {
        let runningAppIDs = self.currentAppInfos()
            .filter { $0.status == .running }
            .map(\.id)

        for appID in runningAppIDs {
            await self.stopApp(id: appID)
        }
    }

    private func ensureLifecycleMutationsAllowed() throws {
        guard !self.isStopping else {
            throw RPCError(code: .failedPrecondition, message: "Wendy Agent is stopping")
        }
    }

    private func registerApp(
        id: String,
        kind: WendyAppInfo.Kind,
        native: WendyApp.NativeMetadata? = nil,
        container: WendyApp.ContainerMetadata? = nil,
        restartPolicy: PersistedRestartPolicy = .default
    ) async throws {
        self.appsByID[id] = WendyApp(
            info: WendyAppInfo(
                id: id,
                kind: kind,
                status: .stopped,
                pid: nil
            ),
            native: native,
            container: container,
            restartPolicy: restartPolicy,
            process: nil,
            launchToken: nil
        )
        try self.saveApps()
        await self.publishApps()
    }

    private func prepareAppForLaunch(id: String, launchToken: UUID) {
        guard var app = self.appsByID[id] else { return }
        app.info = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .stopped,
            pid: nil
        )
        app.process = nil
        app.launchToken = launchToken
        self.appsByID[id] = app
    }

    /// Applies `StartContainer`'s side effects on stored app state: an
    /// explicit start is user intent, so it always clears `stoppedByUser` and
    /// resets the crash-loop counter, and adopts the request's restart
    /// policy only when the request actually carries one (an unset policy
    /// must not clobber a previously stored one).
    private func prepareAppForUserStart(
        id: String,
        requestedPolicy: PersistedRestartPolicy?
    ) {
        guard var app = self.appsByID[id] else { return }
        if let requestedPolicy {
            app.restartPolicy = requestedPolicy
        }
        app.stoppedByUser = false
        app.failureCount = 0
        self.appsByID[id] = app
    }

    private func cancelAppLaunch(id: String, launchToken: UUID) {
        guard var app = self.appsByID[id], app.launchToken == launchToken else { return }
        app.process = nil
        app.launchToken = nil
        self.appsByID[id] = app
    }

    private func markAppRunning(
        id: String,
        process: Foundation.Process,
        launchToken: UUID
    ) async throws {
        guard var app = self.appsByID[id], app.launchToken == launchToken else { return }
        app.info = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .running,
            pid: process.processIdentifier
        )
        app.process = process
        app.launchToken = launchToken
        app.pidBirthTime = self.pidBirthTime(process.processIdentifier)
        self.appsByID[id] = app
        try self.saveApps()
        await self.publishApps()
    }

    private func markAppStopped(id: String, exitCode: Int32? = nil) async {
        guard var app = self.appsByID[id] else { return }

        let stoppedInfo = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .stopped,
            pid: nil
        )
        guard
            app.info != stoppedInfo || app.process != nil || app.launchToken != nil
                || (exitCode != nil && app.lastExitCode != exitCode)
        else {
            return
        }

        app.info = stoppedInfo
        app.process = nil
        app.launchToken = nil
        if let exitCode {
            app.lastExitCode = exitCode
        }
        self.appsByID[id] = app

        do {
            try self.saveApps()
        } catch {
            self.logger.error(
                "Failed to persist stopped app state",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }

        await self.publishApps()
    }

    /// Durably records that the user (not agent shutdown) asked this app to
    /// stop. Only the `StopContainer` RPC calls this — `stopApp`/
    /// `stopAllApps` (the shutdown path) must not, since quitting or
    /// self-updating the agent isn't user intent to keep the app stopped.
    private func markStoppedByUser(id: String) async {
        guard var app = self.appsByID[id], !app.stoppedByUser else { return }
        app.stoppedByUser = true
        self.appsByID[id] = app

        do {
            try self.saveApps()
        } catch {
            self.logger.error(
                "Failed to persist user-stop flag",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }
    }

    func handleAppTermination(id: String, launchToken: UUID, exitCode: Int32? = nil) async {
        guard let app = self.appsByID[id], app.launchToken == launchToken else { return }
        await self.markAppStopped(id: id, exitCode: exitCode)
    }

    private func makeTerminationHandler(
        forAppID id: String,
        launchToken: UUID
    ) -> @Sendable (Foundation.Process) -> Void {
        let service = self
        return { process in
            let exitCode = process.terminationStatus
            Task {
                await service.handleAppTermination(
                    id: id,
                    launchToken: launchToken,
                    exitCode: exitCode
                )
            }
        }
    }

    @discardableResult
    private func stopTrackedAppIfRunning(id: String) async throws -> Bool {
        guard let app = self.appsByID[id], app.info.status == .running else {
            return false
        }

        guard let process = app.process, let launchToken = app.launchToken else {
            // An adopted app: this service didn't launch it, so there is no
            // attached process to terminate. Without these two paths an adopted
            // app could never be stopped at all.
            if app.container != nil, let linuxBackend {
                // The backend owns a container's lifecycle; stop it by name.
                try await linuxBackend.stop(appName: id)
                await self.markAppStopped(id: id)
                return true
            }
            if app.native != nil, let pid = app.info.pid {
                await self.stopAdoptedNativeApp(id: id, pid: pid, app: app)
                return true
            }
            return false
        }

        if !process.isRunning {
            await self.handleAppTermination(
                id: id,
                launchToken: launchToken,
                exitCode: process.terminationStatus
            )
            return true
        }

        let exitTask = Self.makeProcessExitTask(process)

        if app.info.kind == .container, app.container != nil, let linuxBackend {
            try await linuxBackend.stop(appName: id)
            let didExit = await Self.waitForProcessExit(exitTask, timeout: self.nativeStopTimeout)
            if !didExit {
                self.logger.warning(
                    "Container stop timed out, force killing attached process",
                    metadata: ["app_name": "\(id)", "pid": "\(process.processIdentifier)"]
                )
                Self.forceKillProcess(process)
            }
        } else {
            process.terminate()
            let didExit = await Self.waitForProcessExit(exitTask, timeout: self.nativeStopTimeout)
            if !didExit {
                self.logger.warning(
                    "Native app did not exit after terminate, force killing",
                    metadata: ["app_name": "\(id)", "pid": "\(process.processIdentifier)"]
                )
                Self.forceKillProcess(process)
            }
        }

        await exitTask.value
        await self.handleAppTermination(
            id: id,
            launchToken: launchToken,
            exitCode: process.terminationStatus
        )
        return true
    }

    // MARK: - Reconcile & crash supervision

    /// Brings apps back after the agent restarts, and adopts anything that
    /// outlived the previous agent process. Mirrors the Linux agent's
    /// `ReconcileBootContainers` (`go/internal/agent/container/monitor.go:173`):
    /// one immediate pass so apps come back without waiting a supervisor tick.
    ///
    /// Apps the user explicitly stopped, and apps deployed with `--no-restart`,
    /// stay down — that is exactly what `shouldRestart` decides (at this point
    /// `failureCount` is 0 and `lastExitCode` unknown for every app, since both
    /// are runtime-only and reset on load).
    ///
    /// Resilient by design: one app failing to start is logged and the rest of
    /// the pass continues.
    func reconcileApps() async {
        guard !self.isStopping else { return }

        // Survivors first, then the container listing: terminating a survivor
        // can block for seconds, and a listing taken before that wait could
        // report a container as running that has exited by the time it is read,
        // adopting a dead container.
        await self.terminateNativeSurvivors()
        let backendStates = await self.managedContainerStates()

        let candidates = self.appsByID.values
            .sorted { $0.info.id < $1.info.id }
            .filter { Self.shouldRestart($0) }

        guard !candidates.isEmpty else { return }
        self.logger.info(
            "Reconciling apps on start",
            metadata: ["count": "\(candidates.count)"]
        )

        let now = Date()
        for candidate in candidates {
            guard !self.isStopping else { return }
            let id = candidate.info.id
            // Re-read every time: reconcile runs concurrently with the gRPC
            // server, so an RPC can start, stop, or re-register an app across
            // any of the awaits below. Never start an app that is already
            // running (or launching) — that would orphan the live process.
            guard let app = self.appsByID[id],
                app.info.status != .running,
                !Self.isLaunchInFlight(app),
                Self.shouldRestart(app)
            else {
                continue
            }

            if app.container != nil {
                if Self.isRunning(backendStates?[managedContainerName(for: id)]) {
                    // Still running from the previous agent process: adopt it
                    // rather than bouncing a healthy container.
                    await self.adoptRunningApp(id: id, pid: nil)
                    continue
                }
            }

            // Record the start time so the first supervisor tick doesn't
            // immediately restart an app that is still coming up. Unlike the
            // Linux monitor this does NOT bump `failureCount`: a planned agent
            // restart isn't an app failure, and counting it would eat an
            // ON_FAILURE retry on every agent update.
            self.recordAutomaticStart(id: id, at: now)
            await self.startAppUnattended(id: id)
        }
    }

    /// One supervisor tick. Mirrors the Linux monitor's `planRestarts`
    /// (`go/internal/agent/container/monitor.go:370`): skip apps that are
    /// running, skip the ones their policy won't restart, skip anything
    /// restarted inside the 10 s floor, then bump `failureCount`/`lastRestart`
    /// and restart.
    func superviseApps() async {
        guard !self.isStopping else { return }

        // Observed state first, for both app kinds: an adopted app has no
        // attached process, so nothing else notices when it starts or stops.
        // Skipping this is how the restart loop below ends up launching a
        // second copy of something that is already running.
        let backendStates = await self.managedContainerStates()
        await self.syncAdoptedContainerStates(backendStates: backendStates)
        await self.syncAdoptedNativeStates()

        let now = Date()
        for id in self.appsByID.keys.sorted() {
            guard !self.isStopping else { return }
            guard var app = self.appsByID[id] else { continue }
            guard app.info.status != .running, !Self.isLaunchInFlight(app) else { continue }
            guard Self.shouldRestart(app) else { continue }
            if let lastRestart = app.lastRestart,
                now.timeIntervalSince(lastRestart) < self.restartFloorSeconds
            {
                continue
            }

            app.failureCount += 1
            app.lastRestart = now
            self.appsByID[id] = app
            self.logger.info(
                "Restarting app",
                metadata: [
                    "app_name": "\(id)",
                    "failure_count": "\(app.failureCount)",
                    "exit_code": "\(app.lastExitCode.map(String.init) ?? "unknown")",
                ]
            )
            await self.startAppUnattended(id: id)
        }
    }

    /// Mirrors the Linux monitor's `shouldRestart`
    /// (`go/internal/agent/container/monitor.go:434`).
    ///
    /// Intentional divergence on ON_FAILURE: the Linux monitor has no exit-code
    /// signal from containerd and therefore restarts on any exit, which it
    /// documents as a known limitation. The Mac agent owns the child process
    /// and does see the exit code, so ON_FAILURE restarts only after a non-zero
    /// exit. An *unknown* exit code (nil — e.g. right after an agent restart,
    /// where the runtime-only field is cleared) counts as a failure, so
    /// reconcile still brings ON_FAILURE apps back exactly as Linux does.
    private static func shouldRestart(_ app: WendyApp) -> Bool {
        if app.stoppedByUser { return false }
        switch app.restartPolicy.mode {
        case .no:
            return false
        case .unlessStopped:
            return true
        case .onFailure:
            if app.lastExitCode == 0 { return false }
            let maxRetries = app.restartPolicy.onFailureMaxRetries
            if maxRetries > 0, app.failureCount >= maxRetries { return false }
            return true
        }
    }

    /// True while a launch is mid-flight: a token has been claimed but the
    /// process isn't marked running yet. The supervisor must not start a second
    /// copy underneath an in-flight `StartContainer` (a container pull can hold
    /// that window open for a long time).
    private static func isLaunchInFlight(_ app: WendyApp) -> Bool {
        app.launchToken != nil && app.info.status != .running
    }

    /// Starts an app with nobody streaming its RPC response. The pipes still
    /// have to be consumed, so the output is drained into telemetry.
    private func startAppUnattended(id: String) async {
        // Re-checked immediately before the launch (rather than relying solely
        // on callers' own `isStopping` guards) so a tick that was already past
        // its own check when shutdown began cannot start a brand-new launch.
        // This cannot cancel a launch already inside `launchApp` — a container
        // pull is not cancellation-aware — but it closes the window for any
        // launch that has not yet begun.
        guard !self.isStopping else { return }

        do {
            let launched = try await self.launchApp(appName: id)
            self.drainUnattendedOutput(appName: id, launched: launched)
        } catch {
            self.logger.error(
                "Failed to start app automatically",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }
    }

    /// Exposes `startAppUnattended` so a test can exercise its `isStopping`
    /// guard directly, bypassing the (already independently tested) guards in
    /// `superviseApps`/`reconcileApps` that normally sit in front of it.
    func startAppUnattendedForTesting(id: String) async {
        await self.startAppUnattended(id: id)
    }

    /// Consumes an unattended launch's stdout/stderr. Without this the app
    /// blocks as soon as the 64 KB pipe buffer fills; broadcasting the output as
    /// telemetry keeps `wendy device logs` working for auto-started apps.
    private func drainUnattendedOutput(appName: String, launched: LaunchedApp) {
        let broadcaster = self.broadcaster
        let stdout = launched.stdout
        let stderr = launched.stderr
        Task {
            await withTaskGroup(of: Void.self) { group in
                group.addTask {
                    for await data in stdout.fileHandleForReading.bytes(for: appName) {
                        await Self.broadcastLog(
                            broadcaster: broadcaster,
                            appName: appName,
                            text: String(decoding: data, as: UTF8.self),
                            stream: "stdout",
                            severity: .info
                        )
                    }
                }
                group.addTask {
                    for await data in stderr.fileHandleForReading.bytes(for: appName) {
                        await Self.broadcastLog(
                            broadcaster: broadcaster,
                            appName: appName,
                            text: String(decoding: data, as: UTF8.self),
                            stream: "stderr",
                            severity: .warn
                        )
                    }
                }
            }
        }
    }

    /// One `listContainers()` query per pass, keyed by container name. `nil`
    /// when there is no Linux backend, no container app to check, or the query
    /// failed — callers then fall back to the tracked status rather than
    /// guessing that an app is down and restarting a healthy container.
    private func managedContainerStates() async -> [String: String]? {
        guard let linuxBackend,
            self.appsByID.values.contains(where: { $0.container != nil })
        else {
            return nil
        }

        do {
            var states: [String: String] = [:]
            for container in try await linuxBackend.listContainers() {
                // Apple's `container` reports the run name as the id and copies
                // it into `name`; Docker reports a hex id and the name. Key on
                // both so `wendy-<appName>` resolves for either shape.
                if !container.name.isEmpty { states[container.name] = container.state }
                if !container.id.isEmpty, states[container.id] == nil {
                    states[container.id] = container.state
                }
            }
            return states
        } catch {
            self.logger.warning(
                "Failed to list Linux containers while reconciling",
                metadata: ["error": "\(String(describing: error))"]
            )
            return nil
        }
    }

    /// Both backends report a lowercase `running`; every other state (exited,
    /// stopped, created, dead) counts as down. An absent entry — the container
    /// was removed — is down too.
    private static func isRunning(_ state: String?) -> Bool {
        state?.lowercased() == "running"
    }

    /// Folds observed container state into `appsByID` before the restart pass,
    /// for container apps with no attached process (adopted ones): mark a
    /// vanished container stopped, and adopt one that is running while we
    /// believe it isn't, so the tick doesn't needlessly bounce it.
    private func syncAdoptedContainerStates(backendStates: [String: String]?) async {
        guard let backendStates else { return }

        for id in self.appsByID.keys.sorted() {
            guard let app = self.appsByID[id],
                app.container != nil,
                app.process == nil,
                !Self.isLaunchInFlight(app)
            else {
                continue
            }

            let containerIsRunning = Self.isRunning(backendStates[managedContainerName(for: id)])
            switch (app.info.status, containerIsRunning) {
            case (.running, false):
                await self.markAppStopped(id: id)
            case (.stopped, true):
                // Never resurrect the status of an app the user stopped: the
                // backend can still report its container as running for a
                // moment after `stop` returns.
                if !app.stoppedByUser {
                    await self.adoptRunningApp(id: id, pid: nil)
                }
            default:
                break
            }
        }
    }

    /// The native counterpart of `syncAdoptedContainerStates`, and the reason a
    /// warm restart can supervise without reconciling.
    ///
    /// A container app's truth lives outside this service (the backend's
    /// listing), so a rebuilt `ContainerService` re-derives it every tick. A
    /// native app's truth is a pid, and after a warm rebuild — a provisioning
    /// transition builds a fresh service over the same state directory — the
    /// process is still very much alive and owned by the *previous* in-process
    /// service, which holds the real `Process` handle and termination handler.
    /// Without this pass the new service would see a stopped app with a policy
    /// that restarts it and launch a second copy of the same binary one tick
    /// later — deterministically, on every provisioning toggle.
    ///
    /// So: adopt rather than restart, and never signal — the other service owns
    /// that process. This is the deliberate opposite of `reconcileApps`, which
    /// terminates the survivor and relaunches: on a *cold* start nobody owns it,
    /// and relaunching is what gives the agent a handle, a termination handler
    /// and log streaming.
    ///
    /// Identity is the same test the survivor path uses — alive AND executing
    /// this app's binary — so a recycled pid is never adopted.
    private func syncAdoptedNativeStates() async {
        for id in self.appsByID.keys.sorted() {
            guard let app = self.appsByID[id],
                app.native != nil,
                app.process == nil,
                !Self.isLaunchInFlight(app)
            else {
                continue
            }

            switch app.info.status {
            case .running:
                // Adopted on an earlier tick: with no handle, the pid is the
                // only liveness signal there is, so re-check it every tick.
                guard let pid = app.info.pid, self.isPIDRunningApp(pid, app: app) else {
                    await self.markAppStopped(id: id)
                    continue
                }
            case .stopped:
                // Only a pid loaded from `info.json` can identify an app this
                // service never launched. Consumed on the first tick that looks
                // at it, adopted or not.
                guard let pid = app.persistedPID else { continue }
                self.appsByID[id]?.persistedPID = nil
                // Unlike the container case there is no `stoppedByUser` guard:
                // a live pid running this exact binary is definitive evidence,
                // where a backend listing can lag a stop by a moment.
                if self.isPIDRunningApp(pid, app: app) {
                    await self.adoptRunningApp(id: id, pid: pid)
                }
            }
        }
    }

    /// Stops an adopted native app by pid, in the same SIGTERM-then-SIGKILL
    /// shape the process-handle path uses. The identity test is re-run first, so
    /// a pid that has already exited (and possibly been recycled) is marked
    /// stopped without anything being signalled.
    private func stopAdoptedNativeApp(id: String, pid: Int32, app: WendyApp) async {
        guard self.isPIDRunningApp(pid, app: app)
        else {
            await self.markAppStopped(id: id)
            return
        }

        self.sendSignal(pid, SIGTERM)
        if !(await self.waitForPIDToExit(pid, matching: app, timeout: self.nativeStopTimeout)) {
            self.logger.warning(
                "Adopted native app did not exit after SIGTERM, force killing",
                metadata: ["app_name": "\(id)", "pid": "\(pid)"]
            )
            if self.isPIDRunningApp(pid, app: app) { self.sendSignal(pid, SIGKILL) }
            _ = await self.waitForPIDToExit(pid, matching: app, timeout: .seconds(1))
        }

        await self.markAppStopped(id: id)
    }

    /// Whether `pid` is alive AND currently executing this app's binary. The
    /// single identity test shared by adoption, survivor termination and
    /// stopping an adopted app — deliberately strict, so a number the kernel
    /// recycled is never mistaken for the app.
    private func isPIDRunningApp(_ pid: Int32, app: WendyApp) -> Bool {
        guard pid > 0, let binaryPath = Self.nativeBinaryPath(app) else { return false }
        if let expected = app.pidBirthTime {
            return self.pidBirthTime(pid) == expected
        }
        guard let actualPath = self.pidExecutablePath(pid) else { return false }
        return Self.pathsMatch(actualPath, binaryPath)
    }

    nonisolated private static func nativeBinaryPath(_ app: WendyApp) -> String? {
        guard let native = app.native else { return nil }
        return native.executablePath
            ?? (native.binaryName.hasPrefix("/")
                ? native.binaryName : "\(native.directory)/\(native.binaryName)")
    }

    /// Marks an app running without a `Foundation.Process`: it is alive but
    /// this service didn't launch it, so there is no handle to attach. A
    /// container outlived the agent process that started it; a native app is
    /// still owned by a previous in-process `ContainerService` (see
    /// `syncAdoptedNativeStates`). `pid` is the native app's process id, and
    /// `nil` for containers, whose lifecycle the backend owns.
    ///
    /// `stopTrackedAppIfRunning` knows how to stop both shapes.
    private func adoptRunningApp(id: String, pid: Int32?) async {
        guard var app = self.appsByID[id] else { return }

        let runningInfo = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .running,
            pid: pid
        )
        guard app.info != runningInfo || app.process != nil || app.launchToken != nil else {
            return
        }

        app.info = runningInfo
        app.process = nil
        app.launchToken = nil
        self.appsByID[id] = app

        do {
            try self.saveApps()
        } catch {
            self.logger.error(
                "Failed to persist adopted app state",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }

        await self.publishApps()
        self.logger.info(
            "Adopted already-running app",
            metadata: [
                "app_name": "\(id)",
                "kind": "\(app.info.kind.rawValue)",
                "pid": "\(pid.map(String.init) ?? "n/a")",
            ]
        )
    }

    /// Stamps the restart floor without counting a failure — reconcile's start
    /// is not a crash restart.
    private func recordAutomaticStart(id: String, at date: Date) {
        guard var app = self.appsByID[id] else { return }
        app.lastRestart = date
        self.appsByID[id] = app
    }

    /// Terminates every identified native survivor before reconcile starts
    /// anything, forgetting each persisted pid as it is considered.
    ///
    /// Deliberately covers ALL native apps, not just the ones about to be
    /// relaunched: a `--no-restart` app that was running when the agent died is
    /// still running now, while the agent reports it stopped — and a later
    /// `wendy device start` would then launch a second copy that nothing can
    /// see. After this pass, "running" means "the agent launched it".
    private func terminateNativeSurvivors() async {
        for id in self.appsByID.keys.sorted() {
            // Bail out, but leave the pids of the apps not yet looked at in
            // place: they are the only record of those survivors, so discarding
            // them here would make the processes unreclaimable.
            guard !self.isStopping else { return }
            guard let app = self.appsByID[id], app.native != nil, app.persistedPID != nil else {
                continue
            }
            await self.terminateNativeSurvivorIfAny(app)
            // Cleared per app, as it is considered: this pid has served its
            // purpose and a later reconcile must not reconsider it.
            self.appsByID[id]?.persistedPID = nil
        }
    }

    /// Deals with a native app that survived a disorderly agent exit.
    ///
    /// The agent's children are not killed when it dies, so after a crash (or a
    /// `SIGKILL`) the app is still running while this process has no handle on
    /// it. The persisted pid may be that survivor — or a number the kernel has
    /// since handed to something else entirely. It is treated as ours only when
    /// the pid is alive AND its executable path is exactly this app's binary,
    /// and only then is it terminated, so the relaunch that follows leaves
    /// exactly one copy that the agent owns.
    ///
    /// The persisted birth time identifies scripts, interpreters and sandboxed
    /// launches across exec(). A recycled PID with a different birth time is
    /// never adopted or signalled. Legacy snapshots use the executable check.
    ///
    /// Terminating is right here and wrong in `syncAdoptedNativeStates`, which
    /// adopts instead, because of who owns the process: reconcile runs on a
    /// cold start, where the previous agent is gone and nobody owns the
    /// survivor, so relaunching is what gets the agent a handle, a termination
    /// handler and log streaming. A warm rebuild's "survivor" is still owned by
    /// a live `ContainerService` in this same process.
    private func terminateNativeSurvivorIfAny(_ app: WendyApp) async {
        guard let pid = app.persistedPID, pid > 0,
            Self.nativeBinaryPath(app) != nil
        else {
            return
        }
        // Pid reuse cuts both ways: if an RPC has already relaunched this app
        // and the kernel handed it the same number, the "survivor" is the copy
        // the agent is currently running. Never signal that one.
        guard app.info.pid != pid else { return }

        guard self.isPIDRunningApp(pid, app: app) else {
            if let survivorPath = self.pidExecutablePath(pid) {
                self.logger.info(
                    "Ignoring persisted pid: it no longer belongs to this app",
                    metadata: [
                        "app_name": "\(app.info.id)",
                        "pid": "\(pid)",
                        "executable": "\(survivorPath)",
                    ]
                )
            }
            return
        }

        self.logger.notice(
            "Terminating app process that survived a previous agent run",
            metadata: ["app_name": "\(app.info.id)", "pid": "\(pid)"]
        )
        self.sendSignal(pid, SIGTERM)
        if await self.waitForPIDToExit(pid, matching: app, timeout: self.nativeStopTimeout) {
            return
        }

        self.logger.warning(
            "Surviving app process ignored SIGTERM, force killing",
            metadata: ["app_name": "\(app.info.id)", "pid": "\(pid)"]
        )
        if self.isPIDRunningApp(pid, app: app) { self.sendSignal(pid, SIGKILL) }
        let didExit = await self.waitForPIDToExit(
            pid,
            matching: app,
            timeout: .seconds(1)
        )
        if !didExit {
            self.logger.warning(
                "Surviving app process is still alive after SIGKILL; starting a new copy anyway",
                metadata: ["app_name": "\(app.info.id)", "pid": "\(pid)"]
            )
        }
    }

    /// Polls until `pid` stops being the process running `binaryPath`, or the
    /// timeout expires. Checks before sleeping so a pid that is already gone
    /// costs nothing.
    private func waitForPIDToExit(
        _ pid: Int32,
        matching app: WendyApp,
        timeout: Duration
    ) async -> Bool {
        let clock = ContinuousClock()
        let deadline = clock.now + timeout
        while true {
            guard self.isPIDRunningApp(pid, app: app) else {
                return true
            }
            if clock.now >= deadline { return false }
            try? await Task.sleep(for: .milliseconds(50))
        }
    }

    /// `proc_pidpath` reports the fully resolved path, so both sides are
    /// canonicalized before comparing (`/var/folders/...` vs the
    /// `/private/var/folders/...` the kernel reports, symlinked app
    /// directories, and so on).
    nonisolated private static func pathsMatch(_ lhs: String, _ rhs: String) -> Bool {
        Self.canonicalPath(lhs) == Self.canonicalPath(rhs)
    }

    nonisolated private static func canonicalPath(_ path: String) -> String {
        var buffer = [CChar](repeating: 0, count: Int(PATH_MAX))
        guard path.withCString({ realpath($0, &buffer) }) != nil else {
            // Nonexistent path (the common case for a dead survivor's binary):
            // fall back to lexical normalization.
            return URL(fileURLWithPath: path).standardizedFileURL.path
        }
        return Self.string(fromNulTerminated: buffer)
    }

    /// The executable path of a live pid, or `nil` when the pid is not alive or
    /// cannot be inspected. Default implementation behind
    /// `PIDExecutablePathLookup`.
    nonisolated static func executablePath(forPID pid: Int32) -> String? {
        // PROC_PIDPATHINFO_MAXSIZE (4 * PATH_MAX) is what proc_pidpath expects.
        var buffer = [CChar](repeating: 0, count: Int(PATH_MAX) * 4)
        let length = proc_pidpath(pid, &buffer, UInt32(buffer.count))
        guard length > 0 else { return nil }
        return Self.string(fromNulTerminated: buffer)
    }

    /// Decodes a C string an OS call filled into a `[CChar]` buffer.
    /// `String(cString:)`'s array overload is deprecated, so truncate at the NUL
    /// and decode explicitly.
    nonisolated private static func string(fromNulTerminated buffer: [CChar]) -> String {
        let bytes = buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }
        return String(decoding: bytes, as: UTF8.self)
    }

    nonisolated private static func seconds(_ duration: Duration) -> TimeInterval {
        let components = duration.components
        return TimeInterval(components.seconds)
            + TimeInterval(components.attoseconds) / 1_000_000_000_000_000_000
    }

    private func removeApp(id: String) async {
        self.appsByID.removeValue(forKey: id)

        do {
            try self.saveApps()
        } catch {
            self.logger.error(
                "Failed to persist app removal",
                metadata: [
                    "app_name": "\(id)",
                    "error": "\(String(describing: error))",
                ]
            )
        }

        await self.publishApps()
    }

    private func removeNativeAppDirectory(appName: String) throws {
        let appsBaseDirectory = self.appsBase.standardizedFileURL
        let appDirectory = appsBaseDirectory.appendingPathComponent(appName).standardizedFileURL

        guard appDirectory.path.hasPrefix(appsBaseDirectory.path + "/") else {
            self.logger.warning(
                "Skipping native app directory removal outside apps base",
                metadata: [
                    "app_name": "\(appName)",
                    "directory": "\(appDirectory.path)",
                    "apps_base": "\(appsBaseDirectory.path)",
                ]
            )
            return
        }

        guard FileManager.default.fileExists(atPath: appDirectory.path) else { return }
        try FileManager.default.removeItem(at: appDirectory)
        self.logger.info(
            "Native app directory removed",
            metadata: ["app_name": "\(appName)", "directory": "\(appDirectory.path)"]
        )
    }

    nonisolated private static func makeProcessExitTask(
        _ process: Foundation.Process
    ) -> Task<Void, Never> {
        Task.detached {
            process.waitUntilExit()
        }
    }

    nonisolated private static func waitForProcessExit(
        _ exitTask: Task<Void, Never>,
        timeout: Duration
    ) async -> Bool {
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                await exitTask.value
                return true
            }
            group.addTask {
                try? await Task.sleep(for: timeout)
                return false
            }

            let didExit = await group.next() ?? false
            group.cancelAll()
            return didExit
        }
    }

    nonisolated private static func forceKillProcess(_ process: Foundation.Process) {
        guard process.processIdentifier > 0 else { return }
        _ = Darwin.kill(process.processIdentifier, SIGKILL)
    }

    nonisolated static func findBrewExecutable(
        fileExists: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }
    ) -> String? {
        for candidate in ["/opt/homebrew/bin/brew", "/usr/local/bin/brew"] {
            if fileExists(candidate) {
                return candidate
            }
        }
        return nil
    }

    nonisolated static func brewBundleArguments(brewfilePath: String) -> [String] {
        ["bundle", "--file", brewfilePath]
    }

    nonisolated static func realUserName() -> String? {
        guard let passwd = getpwuid(getuid()) else { return nil }
        return String(cString: passwd.pointee.pw_name)
    }

    nonisolated static func brewBundleEnvironment(
        source: [String: String] = ProcessInfo.processInfo.environment,
        overrides: [String: String] = [:],
        realUserName: String? = realUserName()
    ) -> [String: String] {
        var environment: [String: String] = [:]
        for key in ["HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"] {
            if let value = source[key], !value.isEmpty {
                environment[key] = value
            }
        }
        if environment["HOME"] == nil {
            environment["HOME"] = NSHomeDirectory()
        }
        // Homebrew's tap-trust resolves the invoking user's home via a passwd lookup
        // of USER/LOGNAME, so these must name a real account — an env-only identity
        // (as the E2E harness sets) makes every `brew install` abort.
        if let realUserName, !realUserName.isEmpty {
            environment["USER"] = realUserName
            environment["LOGNAME"] = realUserName
        } else {
            for key in ["USER", "LOGNAME"] {
                if let value = source[key], !value.isEmpty {
                    environment[key] = value
                }
            }
        }
        environment["PATH"] = "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin"
        environment["HOMEBREW_NO_ANALYTICS"] = "1"
        return environment
    }

    /// Builds the environment for a native app process. Native apps share the
    /// host network namespace with the agent, so point their OTLP exporter at
    /// the agent's loopback receiver and stamp the resource identity used by
    /// `wendy device logs --app` and attached `wendy run` subscriptions.
    nonisolated static func nativeAppEnvironment(
        appName: String,
        otelPort: Int,
        source: [String: String] = ProcessInfo.processInfo.environment,
        overrides: [String: String] = [:]
    ) -> [String: String] {
        var environment = source.merging(overrides) { _, userValue in userValue }
        environment["WENDY_APP_ID"] = appName
        environment["NSUnbufferedIO"] = "YES"
        environment["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://127.0.0.1:\(otelPort)"
        environment["OTEL_EXPORTER_OTLP_PROTOCOL"] = "grpc"
        for signal in ["LOGS", "METRICS", "TRACES"] {
            environment.removeValue(forKey: "OTEL_EXPORTER_OTLP_\(signal)_ENDPOINT")
            environment.removeValue(forKey: "OTEL_EXPORTER_OTLP_\(signal)_PROTOCOL")
        }
        environment["OTEL_SERVICE_NAME"] = appName

        let appAttribute = "wendy.app.name=\(appName)"
        if let attributes = environment["OTEL_RESOURCE_ATTRIBUTES"], !attributes.isEmpty {
            var foundAppAttribute = false
            let merged = attributes.split(separator: ",").map { attribute -> String in
                let key = attribute.split(separator: "=", maxSplits: 1).first.map(String.init)
                guard key == "wendy.app.name" else { return String(attribute) }
                foundAppAttribute = true
                return appAttribute
            }
            environment["OTEL_RESOURCE_ATTRIBUTES"] =
                (foundAppAttribute
                ? merged
                : merged + [appAttribute]).joined(separator: ",")
        } else {
            environment["OTEL_RESOURCE_ATTRIBUTES"] = appAttribute
        }
        return environment
    }

    nonisolated static func brewBundleFailureMessage(status: Int32) -> String {
        "brew bundle failed with exit code \(status). Run 'wendy device logs --tail 100 --level info' for Homebrew output."
    }

    nonisolated private static func isSafeRelativeBrewfilePath(_ path: String) -> Bool {
        let path = path.hasPrefix("./") ? String(path.dropFirst(2)) : path
        guard !path.isEmpty, !path.hasPrefix("/"), !path.contains("\\"), !path.contains("%") else {
            return false
        }
        guard !path.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
        else { return false }
        for component in path.split(separator: "/", omittingEmptySubsequences: false) {
            if component == "" || component == "." || component == ".." {
                return false
            }
        }
        return true
    }

    nonisolated private static func brewfileURL(
        appDirectory: String,
        brewfile: String
    ) throws -> URL {
        let appDirectoryURL = URL(fileURLWithPath: appDirectory, isDirectory: true)
            .standardizedFileURL
        let brewfileURL = appDirectoryURL.appendingPathComponent(brewfile)
            .standardizedFileURL
        let appComponents = appDirectoryURL.pathComponents
        let brewfileComponents = brewfileURL.pathComponents
        guard brewfileComponents.starts(with: appComponents),
            brewfileComponents.count > appComponents.count
        else {
            throw RPCError(
                code: .invalidArgument,
                message: "brewfile path must stay within the app directory"
            )
        }
        return brewfileURL
    }

    nonisolated private static func ensureNoSymlinkBrewfileComponents(
        appDirectory: String,
        brewfile: String
    ) throws {
        var currentURL = URL(fileURLWithPath: appDirectory, isDirectory: true).standardizedFileURL
        for component in brewfile.split(separator: "/", omittingEmptySubsequences: false) {
            currentURL.appendPathComponent(String(component))
            var statBuffer = stat()
            guard lstat(currentURL.path, &statBuffer) == 0 else { continue }
            if (statBuffer.st_mode & S_IFMT) == S_IFLNK {
                throw RPCError(
                    code: .invalidArgument,
                    message: "brewfile path must stay within the app directory"
                )
            }
        }
    }

    nonisolated private static func validateSyncedBrewfile(atPath path: String) throws {
        let descriptor = open(path, O_RDONLY | O_NOFOLLOW)
        guard descriptor >= 0 else {
            if errno == ELOOP {
                throw RPCError(
                    code: .invalidArgument,
                    message: "brewfile path must stay within the app directory"
                )
            }
            throw RPCError(code: .failedPrecondition, message: "Unable to read Brewfile after sync")
        }
        defer { close(descriptor) }

        var statBuffer = stat()
        guard fstat(descriptor, &statBuffer) == 0 else {
            throw RPCError(
                code: .failedPrecondition,
                message: "Unable to inspect Brewfile after sync"
            )
        }
        guard (statBuffer.st_mode & S_IFMT) == S_IFREG else {
            throw RPCError(code: .invalidArgument, message: "Brewfile must be a regular file")
        }
    }

    nonisolated private static func runBrewBundle(
        brewExecutable: String,
        brewfilePath: String,
        appDirectory: String
    ) async throws -> (status: Int32, output: String, outputTruncated: Bool) {
        try await Task.detached(priority: .utility) {
            let outputDirectory = FileManager.default.temporaryDirectory
                .appendingPathComponent("wendy-brew-output-\(UUID().uuidString)", isDirectory: true)
            try FileManager.default.createDirectory(
                at: outputDirectory,
                withIntermediateDirectories: false,
                attributes: [.posixPermissions: 0o700]
            )
            defer { try? FileManager.default.removeItem(at: outputDirectory) }

            let outputURL = outputDirectory.appendingPathComponent("output.log")
            let outputDescriptor = open(
                outputURL.path,
                O_WRONLY | O_CREAT | O_EXCL,
                S_IRUSR | S_IWUSR
            )
            guard outputDescriptor >= 0 else {
                throw CocoaError(.fileWriteUnknown)
            }

            let outputHandle = FileHandle(fileDescriptor: outputDescriptor, closeOnDealloc: true)
            defer { try? outputHandle.close() }

            let process = Foundation.Process()
            process.executableURL = URL(fileURLWithPath: brewExecutable)
            process.arguments = Self.brewBundleArguments(brewfilePath: brewfilePath)
            process.currentDirectoryURL = URL(fileURLWithPath: appDirectory)
            process.environment = Self.brewBundleEnvironment()
            process.standardOutput = outputHandle
            process.standardError = outputHandle

            try process.run()
            let deadline = Date().addingTimeInterval(5 * 60)
            var timedOut = false
            while process.isRunning {
                if Date() >= deadline {
                    timedOut = true
                    process.terminate()
                    let graceDeadline = Date().addingTimeInterval(2)
                    while process.isRunning && Date() < graceDeadline {
                        try await Task.sleep(for: .milliseconds(50))
                    }
                    if process.isRunning {
                        Self.forceKillProcess(process)
                    }
                    process.waitUntilExit()
                    break
                }
                try await Task.sleep(for: .milliseconds(250))
            }

            try? outputHandle.close()
            let maxOutputBytes = 64 * 1024
            let readHandle = try FileHandle(forReadingFrom: outputURL)
            let data = try readHandle.read(upToCount: maxOutputBytes + 1) ?? Data()
            try? readHandle.close()
            let truncated = data.count > maxOutputBytes
            let outputData = truncated ? data.prefix(maxOutputBytes) : data[...]
            let output = String(decoding: outputData, as: UTF8.self)
                .trimmingCharacters(in: .whitespacesAndNewlines)

            return (timedOut ? 124 : process.terminationStatus, output, truncated)
        }.value
    }

    private func applyBrewfileIfNeeded(
        _ brewfile: String?,
        appName: String,
        appDirectory: String
    ) async throws {
        guard let brewfile = brewfile?.trimmingCharacters(in: .whitespacesAndNewlines),
            !brewfile.isEmpty
        else { return }

        guard Self.isSafeRelativeBrewfilePath(brewfile) else {
            throw RPCError(
                code: .invalidArgument,
                message:
                    "brewfile path must be relative and must not contain '.', '..', or empty components"
            )
        }

        try Self.ensureNoSymlinkBrewfileComponents(appDirectory: appDirectory, brewfile: brewfile)
        let brewfileURL = try Self.brewfileURL(appDirectory: appDirectory, brewfile: brewfile)
        let brewfilePath = brewfileURL.path
        try Self.validateSyncedBrewfile(atPath: brewfilePath)

        guard let brewExecutable = Self.findBrewExecutable() else {
            throw RPCError(
                code: .failedPrecondition,
                message:
                    "Homebrew is required to apply the Brewfile on the target Mac, but brew was not found. Install Homebrew on the target Mac: https://brew.sh/"
            )
        }

        logger.notice(
            "Brewfile package install requested",
            metadata: [
                "app_name": "\(appName)",
                "brew": "\(brewExecutable)",
            ]
        )

        let result: (status: Int32, output: String, outputTruncated: Bool)
        do {
            result = try await Self.runBrewBundle(
                brewExecutable: brewExecutable,
                brewfilePath: brewfilePath,
                appDirectory: appDirectory
            )
        } catch {
            throw RPCError(
                code: .failedPrecondition,
                message: "Failed to launch brew bundle for the Brewfile"
            )
        }

        if result.status == 0, !result.output.isEmpty {
            logger.info(
                "brew bundle output\n\(result.output)\(result.outputTruncated ? "\n[output truncated]" : "")",
                metadata: ["app_name": "\(appName)"]
            )
        }

        guard result.status == 0 else {
            let message = Self.brewBundleFailureMessage(status: result.status)
            logger.error(
                "brew bundle failed\(result.output.isEmpty ? "" : "\n\(result.output)")\(result.outputTruncated ? "\n[output truncated]" : "")",
                metadata: [
                    "app_name": "\(appName)",
                    "exit_code": "\(result.status)",
                ]
            )
            throw RPCError(code: .failedPrecondition, message: message)
        }

        logger.notice(
            "Brewfile package install completed",
            metadata: [
                "app_name": "\(appName)",
                "brew": "\(brewExecutable)",
            ]
        )
    }

    // MARK: - Implemented

    func createContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_CreateContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_CreateContainerResponse> {
        let appName = request.message.appName
        let imageName = request.message.imageName
        logger.info(
            "CreateContainer called",
            metadata: ["app_name": "\(appName)", "image_name": "\(imageName)"]
        )

        try self.ensureLifecycleMutationsAllowed()
        try await self.stopTrackedAppIfRunning(id: appName)

        // Parse app config to determine the target platform.
        let appConfig: WendyAppConfig? = {
            let data = request.message.appConfig
            guard !data.isEmpty else { return nil }
            return try? JSONDecoder().decode(WendyAppConfig.self, from: data)
        }()

        let isLinux =
            appConfig.map { config in
                Self.platformIsLinux(config.platform ?? "linux")
            } ?? false
        let brewfile = appConfig?.brewfile?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""

        if !brewfile.isEmpty, isLinux {
            throw RPCError(
                code: .invalidArgument,
                message: "Brewfile is only supported for native Darwin apps"
            )
        }

        if isLinux {
            guard linuxBackend != nil else {
                throw RPCError(code: .failedPrecondition, message: self.linuxUnavailableMessage)
            }
            try await self.registerApp(
                id: appName,
                kind: .container,
                container: WendyApp.ContainerMetadata(imageName: imageName, appConfig: appConfig),
                restartPolicy: PersistedRestartPolicy(from: request.message.restartPolicy)
            )
            logger.info(
                "Registered Linux container app",
                metadata: ["app_name": "\(appName)", "image": "\(imageName)"]
            )
            return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
        }

        // Native darwin path (existing behavior).

        var nativeLaunchInfo: NativeLaunchInfo
        if imageName.hasPrefix("sha256:") {
            // OCI image: parse manifest → config → extract layer.
            let appDirectory = try NativeProcessConfiguration.contained(
                appName,
                directory: appsBase.path
            )
            try FileManager.default.createDirectory(
                atPath: appDirectory,
                withIntermediateDirectories: true
            )

            // Read manifest blob.
            let manifestData = try readBlob(digest: imageName)
            let manifest = try JSONDecoder().decode(OCIManifest.self, from: manifestData)

            // Read config blob → extract entrypoint.
            let configData = try readBlob(digest: manifest.config.digest)
            let config = try JSONDecoder().decode(OCIImageConfig.self, from: configData)

            guard let entrypoint = config.config?.Entrypoint, let firstEntry = entrypoint.first
            else {
                throw RPCError(code: .invalidArgument, message: "OCI config has no entrypoint")
            }
            // Strip leading "./" from entrypoint to get the binary name.
            let binaryName =
                firstEntry.hasPrefix("./") ? String(firstEntry.dropFirst(2)) : firstEntry

            // Extract layer tarball into app directory.
            guard let layerDesc = manifest.layers.first else {
                throw RPCError(code: .invalidArgument, message: "OCI manifest has no layers")
            }
            try await extractTarGz(blobDigest: layerDesc.digest, to: appDirectory)

            nativeLaunchInfo = NativeLaunchInfo(
                directory: appDirectory,
                binaryName: binaryName,
                args: [],
                currentDirectory: nil
            )
            logger.info(
                "OCI image unpacked",
                metadata: ["app_name": "\(appName)", "binary": "\(binaryName)"]
            )
        } else if !imageName.isEmpty {
            // Legacy: imageName is the binary name directly.
            let appDirectory = try NativeProcessConfiguration.contained(
                appName,
                directory: appsBase.path
            )
            let binaryPath = "\(appDirectory)/\(imageName)"
            nativeLaunchInfo = NativeLaunchInfo(
                directory: appDirectory,
                binaryName: imageName,
                args: [],
                currentDirectory: nil
            )
            logger.info(
                "Registered app directory",
                metadata: ["app_name": "\(appName)", "binary": "\(binaryPath)"]
            )
        } else {
            // File-sync path: imageName is empty, cmd carries the binary name.
            let cmd = request.message.cmd
            guard !cmd.isEmpty else {
                // Nothing to register — container will fall back to --appPath.
                return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
            }
            let appDirectory = try NativeProcessConfiguration.contained(
                appName,
                directory: appsBase.path
            )
            let binaryPath = "\(appDirectory)/\(cmd)"

            nativeLaunchInfo = NativeLaunchInfo(
                directory: appDirectory,
                binaryName: cmd,
                args: Array(request.message.userArgs),
                currentDirectory: appDirectory
            )

            logger.info(
                "Registered app (file-sync path)",
                metadata: ["app_name": "\(appName)", "binary": "\(binaryPath)"]
            )
        }

        try await self.applyBrewfileIfNeeded(
            brewfile,
            appName: appName,
            appDirectory: nativeLaunchInfo.directory
        )
        nativeLaunchInfo.executablePath = try NativeProcessConfiguration.executable(
            nativeLaunchInfo.binaryName,
            directory: nativeLaunchInfo.directory
        )
        nativeLaunchInfo.currentDirectory = try NativeProcessConfiguration.workingDirectory(
            request.message.workingDir,
            directory: nativeLaunchInfo.directory
        )
        nativeLaunchInfo.environment = try NativeProcessConfiguration.environment(
            Array(request.message.env)
        )
        nativeLaunchInfo.args = Array(request.message.userArgs)
        try await self.registerApp(
            id: appName,
            kind: .native,
            native: nativeLaunchInfo,
            restartPolicy: PersistedRestartPolicy(from: request.message.restartPolicy)
        )
        return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
    }

    func startContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StartContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        let appName = request.message.appName
        logger.info("StartContainer called", metadata: ["app_name": "\(appName)"])

        try self.ensureLifecycleMutationsAllowed()
        try await self.stopTrackedAppIfRunning(id: appName)

        guard self.appsByID[appName] != nil else {
            throw RPCError(
                code: .failedPrecondition,
                message: "No registered app found for \(appName). Call CreateContainer first."
            )
        }

        // A request without a policy must not clobber a previously stored
        // one (e.g. `wendy device start` with no --restart flag). Starting is
        // explicit user intent, so it always clears the durable stop flag and
        // gives the app a clean slate for the crash-loop counter.
        self.prepareAppForUserStart(
            id: appName,
            requestedPolicy: request.message.hasRestartPolicy
                ? PersistedRestartPolicy(from: request.message.restartPolicy) : nil
        )

        let launched = try await self.launchApp(appName: appName)
        return self.makeStreamingResponse(
            appName: appName,
            process: launched.process,
            stdoutPipe: launched.stdout,
            stderrPipe: launched.stderr
        )
    }

    /// A process the agent has just launched for an app, together with the
    /// pipes its output has to be read from.
    private struct LaunchedApp {
        let process: Foundation.Process
        let stdout: Pipe
        let stderr: Pipe
    }

    /// Launches `appName` (Linux container through the backend, or a native
    /// Darwin process) and marks it running. Shared by the `StartContainer` RPC
    /// and the reconcile/supervisor paths so an automatic restart takes exactly
    /// the same code path a user-initiated start does.
    ///
    /// The caller owns the returned pipes and MUST consume them: the RPC path
    /// streams them to the client, the unattended path drains them into
    /// telemetry. Left unread, the app blocks once the pipe buffer fills.
    private func launchApp(appName: String) async throws -> LaunchedApp {
        guard let app = self.appsByID[appName] else {
            throw RPCError(
                code: .failedPrecondition,
                message: "No registered app found for \(appName). Call CreateContainer first."
            )
        }

        if let container = app.container {
            guard let linuxBackend else {
                throw RPCError(code: .failedPrecondition, message: self.linuxUnavailableMessage)
            }
            let launchToken = UUID()
            let process: Foundation.Process
            let stdoutPipe: Pipe
            let stderrPipe: Pipe
            // Stored image refs keep what the CLI sent (`localhost:5555/...`,
            // the push listener); backends must pull from the loopback pull
            // listener instead, so rewrite at use time. This also fixes apps
            // installed before the push/pull port split with no migration.
            let imageRef = LocalRegistryRef.rewriteForLocalPull(container.imageName)
            if imageRef != container.imageName {
                logger.info(
                    "Rewriting local registry image ref for on-device pull",
                    metadata: [
                        "app_name": "\(appName)",
                        "from": "\(container.imageName)",
                        "to": "\(imageRef)",
                    ]
                )
            }
            // Claim the launch token BEFORE the pull, not after: the pull is the
            // longest await in this path (minutes for a cold image) and the actor
            // is free throughout it. Until the token is stored the app looks
            // plainly stopped, so `isLaunchInFlight` is false and a supervisor
            // tick would happily start a second copy underneath this one — both
            // racing `createAndStart`, whose delete-then-run would pull the
            // container out from under whichever launch loses `markAppRunning`'s
            // token check. Both catch arms below undo this via `cancelAppLaunch`.
            self.prepareAppForLaunch(id: appName, launchToken: launchToken)
            do {
                // Pull first, then create+start. Both shell out to the Linux
                // runtime CLI, which throws a plain `Error` (e.g. a nonzero exit
                // with stderr) on failure. Wrap those in `RPCError` so the client
                // sees the actionable message — an un-wrapped error surfaces as
                // gRPC's opaque "Service method threw an unknown error." instead.
                try await linuxBackend.pull(image: imageRef)
                (process, stdoutPipe, stderrPipe) = try await linuxBackend.createAndStart(
                    appName: appName,
                    imageName: imageRef,
                    appConfig: container.appConfig,
                    terminationHandler: self.makeTerminationHandler(
                        forAppID: appName,
                        launchToken: launchToken
                    )
                )
            } catch let error as RPCError {
                self.cancelAppLaunch(id: appName, launchToken: launchToken)
                throw error
            } catch {
                self.cancelAppLaunch(id: appName, launchToken: launchToken)
                throw RPCError(
                    code: .internalError,
                    message: "Failed to start Linux container \(appName): \(error)"
                )
            }
            try await self.markAppRunning(id: appName, process: process, launchToken: launchToken)
            logger.info(
                "Container started",
                metadata: ["app_name": "\(appName)", "pid": "\(process.processIdentifier)"]
            )
            return LaunchedApp(process: process, stdout: stdoutPipe, stderr: stderrPipe)
        }

        // Native darwin path.

        // Resolve binary path: prefer uploaded app, fall back to --appPath.
        let binaryPath: String
        let profilePath: String?
        let processArgs: [String]
        let currentDirectory: String?
        if let entry = app.native {
            let directory = try NativeProcessConfiguration.contained(
                entry.directory,
                directory: appsBase.path
            )
            binaryPath = try NativeProcessConfiguration.executable(
                entry.binaryName,
                directory: directory
            )
            let candidateProfile = "\(entry.directory)/sandbox.sb"
            profilePath =
                FileManager.default.fileExists(atPath: candidateProfile)
                ? try NativeProcessConfiguration.contained("sandbox.sb", directory: directory) : nil
            processArgs = entry.args
            currentDirectory = try NativeProcessConfiguration.workingDirectory(
                entry.currentDirectory ?? "",
                directory: entry.directory
            )
        } else {
            binaryPath = executablePath
            profilePath = sandboxProfilePath
            processArgs = []
            currentDirectory = nil
        }

        let process = Foundation.Process()
        // Child stdout/stderr are connected to pipes, not a TTY. Without
        // unbuffered I/O, Swift's `print()` output may sit in stdio buffers for
        // a long time (or until exit), which makes `wendy run` appear silent
        // for long-running native macOS apps like HelloMLX.
        process.environment = Self.nativeAppEnvironment(
            appName: appName,
            otelPort: self.otelPort,
            overrides: app.native?.environment ?? [:]
        )
        if let profilePath {
            process.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
            process.arguments = ["-f", profilePath, binaryPath] + processArgs
            logger.info("Launching \(binaryPath) sandboxed with profile \(profilePath)")
        } else {
            process.executableURL = URL(fileURLWithPath: binaryPath)
            process.arguments = processArgs
            logger.info("Launching \(binaryPath) (not sandboxed)")
        }
        if let currentDirectory {
            process.currentDirectoryURL = URL(fileURLWithPath: currentDirectory)
        }

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        let launchToken = UUID()
        self.prepareAppForLaunch(id: appName, launchToken: launchToken)
        process.terminationHandler = self.makeTerminationHandler(
            forAppID: appName,
            launchToken: launchToken
        )

        do {
            try process.run()
        } catch {
            self.cancelAppLaunch(id: appName, launchToken: launchToken)
            throw RPCError(
                code: .internalError,
                message: "Failed to launch process at \(binaryPath): \(error)"
            )
        }
        try await self.markAppRunning(id: appName, process: process, launchToken: launchToken)
        logger.info(
            "Process started",
            metadata: ["app_name": "\(appName)", "pid": "\(process.processIdentifier)"]
        )

        return LaunchedApp(process: process, stdout: stdoutPipe, stderr: stderrPipe)
    }

    /// Builds the streaming RPC response shared by both the native-process and
    /// Linux-container launch paths: sends a `.started` message, then streams
    /// stdout/stderr as they're produced until the process exits. Output is
    /// drained and broadcast to telemetry on a task whose lifetime belongs to
    /// the launched app, not to this RPC. That keeps logs flowing (and prevents
    /// pipe backpressure) after a detached/watch client disconnects at Started.
    private func makeStreamingResponse(
        appName: String,
        process: Foundation.Process,
        stdoutPipe: Pipe,
        stderrPipe: Pipe
    ) -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        let output = ContainerTaskLifetimeOutput()
        Self.startTaskLifetimeOutputDrain(
            output: output,
            broadcaster: self.broadcaster,
            appName: appName,
            stdoutPipe: stdoutPipe,
            stderrPipe: stderrPipe
        )

        return StreamingServerResponse { writer in
            // The pipe drain deliberately outlives this RPC, but its client-facing
            // stream must not. Finishing here makes later yields no-ops instead of
            // buffering them after a watcher disconnects.
            defer { output.finish() }

            // Send "started" message.
            var started = Wendy_Agent_Services_V1_RunContainerLayersResponse()
            started.responseType = .started(
                Wendy_Agent_Services_V1_RunContainerLayersResponse.Started()
            )
            try await writer.write(started)

            for await response in output.stream {
                try await writer.write(response)
            }

            return Metadata()
        }
    }

    private static func startTaskLifetimeOutputDrain(
        output: ContainerTaskLifetimeOutput,
        broadcaster: TelemetryBroadcaster,
        appName: String,
        stdoutPipe: Pipe,
        stderrPipe: Pipe
    ) {
        Task {
            async let stdoutDrain: Void = Self.drainTaskLifetimeOutput(
                stdoutPipe.fileHandleForReading,
                output: output,
                broadcaster: broadcaster,
                appName: appName,
                fromStdout: true
            )
            async let stderrDrain: Void = Self.drainTaskLifetimeOutput(
                stderrPipe.fileHandleForReading,
                output: output,
                broadcaster: broadcaster,
                appName: appName,
                fromStdout: false
            )
            _ = await (stdoutDrain, stderrDrain)
            output.finish()
        }
    }

    private static func drainTaskLifetimeOutput(
        _ handle: FileHandle,
        output: ContainerTaskLifetimeOutput,
        broadcaster: TelemetryBroadcaster,
        appName: String,
        fromStdout: Bool
    ) async {
        for await data in handle.bytes(for: appName) {
            output.yield(data: data, fromStdout: fromStdout)
            await Self.broadcastLog(
                broadcaster: broadcaster,
                appName: appName,
                text: String(decoding: data, as: UTF8.self),
                stream: fromStdout ? "stdout" : "stderr",
                severity: fromStdout ? .info : .warn
            )
        }
    }

    private static func platformIsLinux(_ platform: String) -> Bool {
        platform == "linux" || platform.hasPrefix("linux/")
            || platform == "wendyos" || platform.hasPrefix("wendyos/")
    }

    func stopContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StopContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_StopContainerResponse> {
        let appName = request.message.appName
        logger.info("StopContainer called", metadata: ["app_name": "\(appName)"])

        // Record user intent before attempting the stop (not inside the
        // shared stop helper, which agent shutdown also uses and must not
        // mark as a user-initiated stop).
        await self.markStoppedByUser(id: appName)

        let didStop = try await self.stopTrackedAppIfRunning(id: appName)
        if didStop {
            if self.appsByID[appName]?.info.kind == .container {
                logger.info("Linux container stopped", metadata: ["app_name": "\(appName)"])
            } else {
                logger.info("Process stopped", metadata: ["app_name": "\(appName)"])
            }
        } else {
            logger.warning("No running process found", metadata: ["app_name": "\(appName)"])
        }

        return ServerResponse(message: Wendy_Agent_Services_V1_StopContainerResponse())
    }

    func deleteContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_DeleteContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DeleteContainerResponse> {
        let appName = request.message.appName
        logger.info("DeleteContainer called", metadata: ["app_name": "\(appName)"])

        try await self.stopTrackedAppIfRunning(id: appName)

        if self.appsByID[appName]?.container != nil, let linuxBackend {
            try await linuxBackend.remove(appName: appName)
            await self.removeApp(id: appName)
            logger.info("Linux container removed", metadata: ["app_name": "\(appName)"])
        } else {
            try self.removeNativeAppDirectory(appName: appName)
            await self.removeApp(id: appName)
        }

        return ServerResponse(message: Wendy_Agent_Services_V1_DeleteContainerResponse())
    }

    func listContainers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListContainersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_ListContainersResponse> {
        // Capture entitlement-derived ports and running state while still on
        // the actor: appsByID is actor-isolated state, but the
        // StreamingServerResponse closure below is not guaranteed to run on
        // the actor's executor.
        let containers = self.appsByID.values
            .sorted { $0.info.id < $1.info.id }
            .map { app -> AppContainer in
                var container = AppContainer()
                container.appName = app.info.id
                container.runningState = Self.runningState(for: app)
                let (http, mcp) = self.entitlementPorts(forAppID: app.info.id)
                container.httpPort = http
                container.mcpPort = mcp
                return container
            }

        return StreamingServerResponse { writer in
            for container in containers {
                var response = Wendy_Agent_Services_V1_ListContainersResponse()
                response.container = container
                try await writer.write(response)
            }

            return Metadata()
        }
    }

    /// Reads the http/mcp entitlement ports declared in the app's retained
    /// wendy.json config, if it has one (native/file-sync apps have no
    /// `.container` metadata and no entitlements, so both are 0 for them).
    private func entitlementPorts(forAppID appID: String) -> (http: UInt32, mcp: UInt32) {
        guard let entitlements = appsByID[appID]?.container?.appConfig?.entitlements else {
            return (0, 0)
        }
        var http: UInt32 = 0
        var mcp: UInt32 = 0
        for entitlement in entitlements {
            // Ports are validated 1-65535 on the Go/CLI side, but wendy.json is
            // decoded independently here with no range check on `port` (an
            // arbitrary Int). UInt32(port), unlike Go's uint32(port), traps on
            // out-of-range input instead of wrapping — so a value outside
            // 1-65535 must be treated the same as port <= 0 (silently skipped),
            // not converted, or listContainers crashes every time it's called
            // for as long as the bad config is persisted.
            guard let port = entitlement.port, port > 0, port <= 65535 else { continue }
            switch entitlement.type {
            case "http" where http == 0:
                http = UInt32(port)
            case "mcp" where mcp == 0:
                mcp = UInt32(port)
            default:
                break
            }
            if http != 0 && mcp != 0 {
                break
            }
        }
        return (http, mcp)
    }

    /// A down app the supervisor has already restarted at least once and will
    /// restart again reports `CRASH_LOOPING` rather than `STOPPED`, so a crash
    /// loop is visible instead of masquerading as an ordinary stop. A
    /// user-stopped app, or one whose policy won't restart it, stays `STOPPED`.
    private static func runningState(for app: WendyApp) -> AppRunningState {
        if app.info.status == .running { return .running }
        if app.failureCount > 0, Self.shouldRestart(app) { return .crashLooping }
        return .stopped
    }

    func listContainerStats(
        request: ServerRequest<Wendy_Agent_Services_V1_ListContainerStatsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListContainerStatsResponse> {
        let appNames = Set(appsByID.keys)
            .sorted()

        var response = Wendy_Agent_Services_V1_ListContainerStatsResponse()
        response.stats = appNames.map { appName in
            var stats = Wendy_Agent_Services_V1_ContainerStats()
            stats.appName = appName
            if let pid = appsByID[appName]?.info.pid,
                let sample = SystemStats.processStats(pid: pid)
            {
                stats.memoryBytes = sample.memoryBytes
            }
            return stats
        }
        return ServerResponse(message: response)
    }

    // MARK: - Unsupported on macOS

    func attachContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_AttachContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Linux container attach is currently not supported by Wendy Agent for Mac."
        )
    }

    func execContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_ExecContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_ExecContainerResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Interactive container exec is currently not supported by Wendy Agent for Mac."
        )
    }

    func listVolumes(
        request: ServerRequest<Wendy_Agent_Services_V1_ListVolumesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListVolumesResponse> {
        throw RPCError(
            code: .unimplemented,
            message:
                "Container volume management is currently not supported by Wendy Agent for Mac."
        )
    }

    func removeVolume(
        request: ServerRequest<Wendy_Agent_Services_V1_RemoveVolumeRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_RemoveVolumeResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Removing container volumes is currently not supported by Wendy Agent for Mac."
        )
    }

    func getResourceStats(
        request: ServerRequest<Wendy_Agent_Services_V1_GetResourceStatsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetResourceStatsResponse> {
        let host = SystemStats.hostStats()
        var response = Wendy_Agent_Services_V1_GetResourceStatsResponse()

        var hostStats = Wendy_Agent_Services_V1_HostStats()
        hostStats.cpuTotalJiffies = host.cpuTotalTicks
        hostStats.cpuIdleJiffies = host.cpuIdleTicks
        hostStats.cpuCount = host.cpuCount
        hostStats.memTotalBytes = host.memTotalBytes
        hostStats.memAvailableBytes = host.memAvailableBytes
        response.host = hostStats

        response.containers = appsByID.keys.sorted().compactMap { appName in
            guard let pid = appsByID[appName]?.info.pid,
                let sample = SystemStats.processStats(pid: pid)
            else { return nil }
            var container = Wendy_Agent_Services_V1_ResourceContainerStats()
            container.appName = appName
            container.cpuUsageNanos = sample.cpuUsageNanos
            container.memoryBytes = sample.memoryBytes
            return container
        }
        return ServerResponse(message: response)
    }

    func streamMCP(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_MCPChunk>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_MCPChunk> {
        throw RPCError(
            code: .unimplemented,
            message: "MCP streaming is currently not supported by Wendy Agent for Mac."
        )
    }

    func getContainerPorts(
        request: ServerRequest<Wendy_Agent_Services_V1_GetContainerPortsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetContainerPortsResponse> {
        var response = Wendy_Agent_Services_V1_GetContainerPortsResponse()
        if let pid = appsByID[request.message.appName]?.info.pid {
            let ports = await SystemStats.listeningPorts(pid: pid)
            response.ports = ports.map { sample in
                var entry = Wendy_Agent_Services_V1_PortEntry()
                entry.protocol = sample.proto
                entry.port = sample.port
                entry.address = sample.address
                return entry
            }
        }
        return ServerResponse(message: response)
    }

    func queryChunks(
        request: ServerRequest<Wendy_Agent_Services_V1_QueryChunksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_QueryChunksResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Chunk-level layer transfer is currently not supported by Wendy Agent for Mac."
        )
    }

    func writeChunks(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_WriteChunksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_WriteChunksResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Chunk-level layer transfer is currently not supported by Wendy Agent for Mac."
        )
    }

    func queryLayers(
        request: ServerRequest<Wendy_Agent_Services_V1_QueryLayersRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_QueryLayersResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Layer content queries are currently not supported by Wendy Agent for Mac."
        )
    }

    func listLayers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_LayerHeader> {
        throw RPCError(
            code: .unimplemented,
            message: "Container layer listing is currently not supported by Wendy Agent for Mac."
        )
    }

    func writeLayer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_WriteLayerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_WriteLayerResponse> {
        var digestStr = ""
        var accumulated = Data()

        for try await message in request.messages {
            if !message.digest.isEmpty && digestStr.isEmpty {
                digestStr = message.digest
            }
            if !message.data.isEmpty {
                accumulated.append(message.data)
            }
        }

        guard !digestStr.isEmpty else {
            throw RPCError(
                code: .invalidArgument,
                message: "No digest received in WriteLayer stream"
            )
        }

        // Detect format: "sha256:<hex>" (OCI, 2 parts) vs "<app>:<file>:sha256:<hex>" (legacy, 4 parts).
        let parts = digestStr.split(separator: ":", maxSplits: 3).map(String.init)

        if parts.count == 2 && parts[0] == "sha256" {
            // OCI blob format.
            let expectedHash = parts[1]
            let computedHash = SHA256.hash(data: accumulated)
                .map { String(format: "%02x", $0) }
                .joined()
            guard computedHash == expectedHash else {
                throw RPCError(
                    code: .dataLoss,
                    message: "SHA256 mismatch: expected \(expectedHash), got \(computedHash)"
                )
            }

            let blobURL = try validateContainedPath(
                base: URL(fileURLWithPath: blobsDirectory),
                relative: "sha256/\(expectedHash)"
            )
            try accumulated.write(to: blobURL)

            logger.info(
                "WriteLayer completed (OCI blob)",
                metadata: [
                    "digest": "\(digestStr)",
                    "size": "\(accumulated.count)",
                ]
            )
        } else if parts.count == 4 && parts[2] == "sha256" {
            // Legacy format: "<appName>:<filename>:sha256:<hash>".
            let appName = parts[0]
            let filename = parts[1]
            let expectedHash = parts[3]

            let computedHash = SHA256.hash(data: accumulated)
                .map { String(format: "%02x", $0) }
                .joined()
            guard computedHash == expectedHash else {
                throw RPCError(
                    code: .dataLoss,
                    message: "SHA256 mismatch: expected \(expectedHash), got \(computedHash)"
                )
            }

            let appDirectoryURL = try validateContainedPath(base: appsBase, relative: appName)
            try FileManager.default.createDirectory(
                at: appDirectoryURL,
                withIntermediateDirectories: true
            )
            let fileURL = try validateContainedPath(base: appDirectoryURL, relative: filename)
            try accumulated.write(to: fileURL)

            if filename != "sandbox.sb" {
                try FileManager.default.setAttributes(
                    [.posixPermissions: 0o755],
                    ofItemAtPath: fileURL.path
                )
            }

            logger.info(
                "WriteLayer completed (legacy)",
                metadata: [
                    "app_name": "\(appName)",
                    "filename": "\(filename)",
                    "size": "\(accumulated.count)",
                ]
            )
        } else {
            throw RPCError(code: .invalidArgument, message: "Invalid digest format: \(digestStr)")
        }

        return StreamingServerResponse { _ in
            return Metadata()
        }
    }

    func createContainerWithProgress(
        request: ServerRequest<Wendy_Agent_Services_V1_CreateContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<
        Wendy_Agent_Services_V1_CreateContainerProgressResponse
    > {
        throw RPCError(
            code: .unimplemented,
            message:
                "Container creation progress streaming is currently not supported by Wendy Agent for Mac."
        )
    }

    func runContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_RunContainerLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        throw RPCError(
            code: .unimplemented,
            message:
                "Legacy container streaming execution is currently not supported by Wendy Agent for Mac. Use the native app lifecycle RPCs instead when applicable."
        )
    }

    // MARK: - Helpers

    private func readBlob(digest: String) throws -> Data {
        // digest is "sha256:<hex>" — map to blobsDirectory/sha256/<hex>.
        let blobPath = try validateContainedPath(
            base: URL(fileURLWithPath: blobsDirectory),
            relative: digest.replacingOccurrences(of: ":", with: "/")
        ).path
        guard let data = FileManager.default.contents(atPath: blobPath) else {
            throw RPCError(code: .notFound, message: "Blob not found at \(blobPath)")
        }
        return data
    }

    private func extractTarGz(blobDigest: String, to destinationDirectory: String) async throws {
        let blobPath = try validateContainedPath(
            base: URL(fileURLWithPath: blobsDirectory),
            relative: blobDigest.replacingOccurrences(of: ":", with: "/")
        ).path
        let tarProcess = Foundation.Process()
        tarProcess.executableURL = URL(fileURLWithPath: "/usr/bin/tar")
        tarProcess.arguments = ["-xzf", blobPath, "-C", destinationDirectory]

        let status = try await withCheckedThrowingContinuation {
            (continuation: CheckedContinuation<Int32, any Error>) in
            tarProcess.terminationHandler = { p in
                continuation.resume(returning: p.terminationStatus)
            }
            do {
                try tarProcess.run()
            } catch {
                continuation.resume(throwing: error)
            }
        }

        guard status == 0 else {
            throw RPCError(
                code: .internalError,
                message: "tar extraction failed with status \(status)"
            )
        }
    }

    private static func broadcastLog(
        broadcaster: TelemetryBroadcaster,
        appName: String,
        text: String,
        stream: String,
        severity: Opentelemetry_Proto_Logs_V1_SeverityNumber
    ) async {
        let timestamp = UInt64(Date().timeIntervalSince1970 * 1_000_000_000)
        await broadcaster.broadcastLogs(
            Self.containerLogRequest(
                appName: appName,
                text: text,
                stream: stream,
                severity: severity,
                timestamp: timestamp
            )
        )
    }

    /// Produces the canonical OTLP representation of adapted process output.
    /// The instrumentation scope is the discriminator CLI clients use to avoid
    /// displaying this record once from the process stream and again from OTLP.
    nonisolated static func containerLogRequest(
        appName: String,
        text: String,
        stream: String,
        severity: Opentelemetry_Proto_Logs_V1_SeverityNumber,
        timestamp: UInt64
    ) -> Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest {

        var logRecord = Opentelemetry_Proto_Logs_V1_LogRecord()
        logRecord.timeUnixNano = timestamp
        logRecord.observedTimeUnixNano = timestamp
        logRecord.severityNumber = severity
        logRecord.severityText = severity == .info ? "INFO" : "WARN"
        logRecord.body = .with { $0.stringValue = text }
        logRecord.attributes.append(
            .with {
                $0.key = "stream"
                $0.value = .with { $0.stringValue = stream }
            }
        )

        var scopeLogs = Opentelemetry_Proto_Logs_V1_ScopeLogs()
        scopeLogs.scope.name = "wendy.container"
        scopeLogs.logRecords = [logRecord]

        var resourceLogs = Opentelemetry_Proto_Logs_V1_ResourceLogs()
        resourceLogs.scopeLogs = [scopeLogs]
        resourceLogs.resource.attributes.append(
            .with {
                $0.key = "service.name"
                $0.value = .with { $0.stringValue = appName }
            }
        )
        resourceLogs.resource.attributes.append(
            .with {
                $0.key = "wendy.app.name"
                $0.value = .with { $0.stringValue = appName }
            }
        )

        return Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest.with {
            $0.resourceLogs = [resourceLogs]
        }
    }
}

// MARK: - FileHandle async bytes helper

extension FileHandle {
    /// Read available data from the file handle as an async sequence of chunks.
    func bytes(for label: String) -> AsyncStream<Data> {
        AsyncStream { continuation in
            self.readabilityHandler = { handle in
                let data = handle.availableData
                if data.isEmpty {
                    continuation.finish()
                    handle.readabilityHandler = nil
                } else {
                    continuation.yield(data)
                }
            }
        }
    }
}
