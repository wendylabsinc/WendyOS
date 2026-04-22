import CryptoKit
import Foundation
import GRPCCore
import Logging
import OpenTelemetryGRPC
import Subprocess
import WendyAgentGRPC

actor ContainerService: Wendy_Agent_Services_V1_WendyContainerService.ServiceProtocol {
    private let appsBase: URL
    private let blobsDirectory: String
    private let broadcaster: TelemetryBroadcaster
    private let infoFileURL: URL
    private let onAppsChanged: @Sendable ([WendyAppInfo]) async -> Void
    private typealias NativeLaunchInfo = WendyApp.NativeMetadata

    private let executablePath: String
    private let logger = Logger(label: "sh.wendy.agent.container")
    private let nativeStopTimeout: Duration = .seconds(5)
    private var appsByID: [String: WendyApp] = [:]
    private var isStopping = false
    private let sandboxProfilePath: String?

    /// Path to the `docker` CLI binary.
    ///
    /// Defaults to resolving `docker` on `PATH`. Tests inject a path to a
    /// fake shell script. Whether docker is actually usable at runtime is
    /// discovered separately by ``ensureReady()``.
    private let dockerExecutable: String

    /// Whether the docker CLI is usable and the local registry container
    /// is running. Set by ``ensureReady()`` during startup and consulted
    /// by the Linux-container RPC paths.
    private var dockerPresent: Bool = false
    private var dockerReadyProbed: Bool = false

    init(
        broadcaster: TelemetryBroadcaster,
        executablePath: String,
        sandboxProfilePath: String? = nil,
        stateDirectory: URL? = nil,
        appsBase: URL? = nil,
        dockerExecutable: String = "docker",
        onAppsChanged: @escaping @Sendable ([WendyAppInfo]) async -> Void = { _ in }
    ) {
        self.broadcaster = broadcaster
        self.onAppsChanged = onAppsChanged
        self.executablePath = executablePath
        self.sandboxProfilePath = sandboxProfilePath
        self.dockerExecutable = dockerExecutable

        let defaultStateDirectory = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/wendy-agent")
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
        container: WendyApp.ContainerMetadata? = nil
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
        app.dockerRunTask = nil
        app.launchToken = launchToken
        self.appsByID[id] = app
    }

    private func cancelAppLaunch(id: String, launchToken: UUID) {
        guard var app = self.appsByID[id], app.launchToken == launchToken else { return }
        app.process = nil
        app.dockerRunTask = nil
        app.launchToken = nil
        self.appsByID[id] = app
    }

    private func markAppRunning(
        id: String,
        pid: Int32,
        nativeProcess: Foundation.Process? = nil,
        dockerRunTask: Task<Void, Never>? = nil,
        launchToken: UUID
    ) async throws {
        guard var app = self.appsByID[id], app.launchToken == launchToken else { return }
        app.info = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .running,
            pid: pid
        )
        app.process = nativeProcess
        app.dockerRunTask = dockerRunTask
        app.launchToken = launchToken
        self.appsByID[id] = app
        try self.saveApps()
        await self.publishApps()
    }

    private func markAppStopped(id: String) async {
        guard var app = self.appsByID[id] else { return }

        let stoppedInfo = WendyAppInfo(
            id: app.info.id,
            kind: app.info.kind,
            status: .stopped,
            pid: nil
        )
        guard
            app.info != stoppedInfo
                || app.process != nil
                || app.dockerRunTask != nil
                || app.launchToken != nil
        else {
            return
        }

        app.info = stoppedInfo
        app.process = nil
        app.dockerRunTask = nil
        app.launchToken = nil
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

    func handleAppTermination(id: String, launchToken: UUID) async {
        guard let app = self.appsByID[id], app.launchToken == launchToken else { return }
        await self.markAppStopped(id: id)
    }

    private func makeTerminationHandler(
        forAppID id: String,
        launchToken: UUID
    ) -> @Sendable (Foundation.Process) -> Void {
        let service = self
        return { _ in
            Task {
                await service.handleAppTermination(id: id, launchToken: launchToken)
            }
        }
    }

    @discardableResult
    private func stopTrackedAppIfRunning(id: String) async throws -> Bool {
        guard let app = self.appsByID[id],
            let launchToken = app.launchToken,
            app.info.status == .running
        else {
            return false
        }

        if let process = app.process {
            // Native darwin path.
            if !process.isRunning {
                await self.handleAppTermination(id: id, launchToken: launchToken)
                return true
            }

            let exitTask = Self.makeProcessExitTask(process)
            process.terminate()
            let didExit = await Self.waitForTaskExit(exitTask, timeout: self.nativeStopTimeout)
            if !didExit {
                self.logger.warning(
                    "Native app did not exit after terminate, force killing",
                    metadata: ["app_name": "\(id)", "pid": "\(process.processIdentifier)"]
                )
                Self.forceKillProcess(process)
            }
            await exitTask.value
            await self.handleAppTermination(id: id, launchToken: launchToken)
            return true
        }

        if let runTask = app.dockerRunTask, self.dockerPresent {
            // Attached docker path.
            let containerName = Self.containerName(forAppID: id)
            _ = try? await self.runDocker(
                ["stop", "--time", "10", containerName]
            )
            let didExit = await Self.waitForTaskExit(runTask, timeout: self.nativeStopTimeout)
            if !didExit {
                self.logger.warning(
                    "Docker stop timed out, force-killing container and cancelling attached run",
                    metadata: ["app_name": "\(id)", "container": "\(containerName)"]
                )
                _ = try? await self.runDocker(["kill", containerName])
                runTask.cancel()
                await runTask.value
            }
            await self.handleAppTermination(id: id, launchToken: launchToken)
            return true
        }

        // No runtime handle to stop. Just mark stopped.
        await self.handleAppTermination(id: id, launchToken: launchToken)
        return true
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

    nonisolated private static func makeProcessExitTask(
        _ process: Foundation.Process
    ) -> Task<Void, Never> {
        Task.detached {
            process.waitUntilExit()
        }
    }

    nonisolated private static func waitForTaskExit(
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

    // MARK: - Docker helpers

    /// Derive the Docker container name for a given Wendy app id.
    nonisolated private static func containerName(forAppID appID: String) -> String {
        "wendy-\(appID)"
    }

    /// Translate Wendy entitlements into `docker run` argv flags.
    ///
    /// Throws an `RPCError` if any entitlement cannot be honored, so callers
    /// fail fast rather than launching a container that silently lacks the
    /// capabilities the app was promised.
    nonisolated private static func dockerRunArgs(
        from entitlements: [WendyEntitlement],
        appName: String
    ) throws -> [String] {
        var args: [String] = []

        for entitlement in entitlements {
            switch entitlement.type {
            case "network":
                if entitlement.mode == "none" {
                    args += ["--network", "none"]
                } else if let ports = entitlement.ports {
                    // --network=host doesn't work on Docker Desktop for Mac, so
                    // map explicit ports from the entitlement's ports array.
                    for port in ports {
                        args += ["-p", "\(port.host):\(port.container)"]
                    }
                }

            case "persist":
                if let name = entitlement.name, let path = entitlement.path {
                    args += ["-v", "wendy-\(appName)-\(name):\(path)"]
                }

            case "gpu", "bluetooth", "audio", "video", "camera", "usb", "i2c", "gpio":
                throw RPCError(
                    code: .failedPrecondition,
                    message:
                        "Entitlement '\(entitlement.type)' is not available for Linux containers on macOS (VM isolation). Refusing to launch '\(appName)'."
                )

            default:
                throw RPCError(
                    code: .invalidArgument,
                    message:
                        "Unknown entitlement type '\(entitlement.type)' requested by '\(appName)'."
                )
            }
        }

        return args
    }

    // MARK: - Docker startup

    /// Host port for the local Docker registry. Uses 5555 instead of the
    /// default 5000 to avoid conflicts with macOS AirPlay Receiver, which
    /// binds `*:5000` by default on every Mac.
    static let registryPort: UInt16 = 5555

    /// Upper bound on collected stdout/stderr bytes for a single one-shot
    /// docker invocation. `swift-subprocess` drains output concurrently, so
    /// this cap only prevents unbounded accumulation if docker misbehaves.
    private static let maxCollectedOutputBytes = 16 * 1024 * 1024

    /// Probe whether docker is usable, and if so ensure the local registry
    /// container is running. Called once at startup before the service begins
    /// handling RPC traffic.
    func ensureReady() async {
        guard !self.dockerReadyProbed else { return }
        self.dockerReadyProbed = true

        do {
            _ = try await self.runDocker(
                ["version", "--format", "{{.Server.Version}}"],
                timeout: .seconds(5)
            )
        } catch {
            self.logger.info("Docker not available, Linux container support disabled")
            return
        }

        do {
            try await self.ensureRegistryRunning()
            self.dockerPresent = true
        } catch {
            self.logger.warning(
                "Failed to start Docker registry: \(String(describing: error)). Linux container support disabled."
            )
        }
    }

    /// Make sure the `wendy-registry` container is running.
    private func ensureRegistryRunning() async throws {
        let psOutput = try await self.runDocker(
            ["ps", "--filter", "name=wendy-registry", "--format", "{{.Status}}"],
            timeout: .seconds(5)
        )
        if psOutput.contains("Up") {
            return
        }

        // Remove stale container if present, then start a fresh one.
        _ = try? await self.runDocker(
            ["rm", "-f", "wendy-registry"],
            timeout: .seconds(5)
        )
        _ = try await self.runDocker(
            [
                "run", "-d",
                "-p", "\(Self.registryPort):5000",
                "--name", "wendy-registry",
                "--restart", "unless-stopped",
                "registry:2",
            ],
            timeout: .seconds(5)
        )
    }

    /// Run a `docker` command and return its trimmed stdout.
    ///
    /// Uses `swift-subprocess`, which drains stdout/stderr concurrently while
    /// the child runs (no `readDataToEndOfFile` at termination, no unbounded
    /// buffering). Non-zero exit, signal termination, timeout, and spawn
    /// failure all surface as `RPCError` so callers can propagate them to
    /// gRPC without further translation.
    private func runDocker(
        _ args: [String],
        timeout: Duration? = nil
    ) async throws -> String {
        let executable = self.dockerExecutable
        let runCommand: @Sendable () async throws -> String = {
            let record: ExecutionRecord<StringOutput<UTF8>, StringOutput<UTF8>>
            do {
                record = try await Subprocess.run(
                    .name(executable),
                    arguments: Arguments(args),
                    output: .string(limit: Self.maxCollectedOutputBytes),
                    error: .string(limit: Self.maxCollectedOutputBytes)
                )
            } catch let error as SubprocessError {
                throw RPCError(
                    code: .internalError,
                    message: "docker \(args.joined(separator: " ")) failed to launch: \(error)"
                )
            }

            let stdout = (record.standardOutput ?? "")
                .trimmingCharacters(in: .whitespacesAndNewlines)

            switch record.terminationStatus {
            case .exited(0):
                return stdout
            case .exited(let code):
                let stderr = (record.standardError ?? "")
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                throw RPCError(
                    code: .internalError,
                    message: stderr.isEmpty
                        ? "docker \(args.joined(separator: " ")) exited with status \(code)"
                        : "docker \(args.joined(separator: " ")) exited with status \(code): \(stderr)"
                )
            case .signaled(let signal):
                let stderr = (record.standardError ?? "")
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                throw RPCError(
                    code: .internalError,
                    message: stderr.isEmpty
                        ? "docker \(args.joined(separator: " ")) terminated by signal \(signal)"
                        : "docker \(args.joined(separator: " ")) terminated by signal \(signal): \(stderr)"
                )
            }
        }

        guard let timeout else {
            return try await runCommand()
        }

        do {
            return try await withThrowingTaskGroup(of: String.self) { group in
                group.addTask { try await runCommand() }
                group.addTask {
                    try await Task.sleep(for: timeout)
                    throw RPCError(
                        code: .deadlineExceeded,
                        message:
                            "docker \(args.joined(separator: " ")) timed out after \(timeout)"
                    )
                }

                defer { group.cancelAll() }

                guard let result = try await group.next() else {
                    throw RPCError(
                        code: .internalError,
                        message: "docker \(args.joined(separator: " ")) produced no result"
                    )
                }
                return result
            }
        } catch {
            if let rpc = error as? RPCError, rpc.code == .deadlineExceeded {
                self.logger.warning(
                    "Docker command timed out",
                    metadata: [
                        "command": "\(([executable] + args).joined(separator: " "))",
                        "timeout": "\(timeout)",
                    ]
                )
            }
            throw error
        }
    }

    /// Test-only shim that exposes the docker invocation pipeline so tests
    /// can exercise timeout, non-zero exit, and large-output behavior without
    /// going through a full RPC path.
    internal func runDockerForTesting(
        _ args: [String],
        timeout: Duration? = nil
    ) async throws -> String {
        try await self.runDocker(args, timeout: timeout)
    }

    /// The handle for an in-flight attached `docker run`.
    private struct AttachedDockerRun {
        let pid: Int32
        let runTask: Task<Void, Never>
        let stdout: AsyncThrowingStream<Data, any Error>
        let stderr: AsyncThrowingStream<Data, any Error>
    }

    /// Launch `docker run` attached via `swift-subprocess` and return the PID,
    /// the task that owns the subprocess lifetime, and the stdout/stderr
    /// streams the gRPC layer forwards to the client.
    ///
    /// This function only returns after the subprocess has entered the
    /// streaming closure (so the PID is real) or after spawn has failed (in
    /// which case it throws). That handshake lets callers mark the app as
    /// running with a valid PID before returning to gRPC.
    private func launchAttachedDockerRun(
        args: [String],
        appName: String,
        launchToken: UUID
    ) async throws -> AttachedDockerRun {
        let (stdoutStream, stdoutCont) =
            AsyncThrowingStream<Data, any Error>.makeStream()
        let (stderrStream, stderrCont) =
            AsyncThrowingStream<Data, any Error>.makeStream()
        let (spawnStream, spawnCont) =
            AsyncThrowingStream<Int32, any Error>.makeStream()

        let dockerExecutable = self.dockerExecutable

        let runTask: Task<Void, Never> = Task.detached { [weak self] in
            do {
                _ = try await Subprocess.run(
                    .name(dockerExecutable),
                    arguments: Arguments(args)
                ) { execution, _, stdoutSeq, stderrSeq in
                    spawnCont.yield(execution.processIdentifier.value)
                    spawnCont.finish()

                    try await withThrowingTaskGroup(of: Void.self) { group in
                        group.addTask {
                            for try await buffer in stdoutSeq {
                                let data = buffer.withUnsafeBytes { Data($0) }
                                stdoutCont.yield(data)
                            }
                        }
                        group.addTask {
                            for try await buffer in stderrSeq {
                                let data = buffer.withUnsafeBytes { Data($0) }
                                stderrCont.yield(data)
                            }
                        }
                        try await group.waitForAll()
                    }
                }
                stdoutCont.finish()
                stderrCont.finish()
            } catch {
                // If spawn failed we haven't yielded yet; propagate the error
                // through the spawn handshake. If we have yielded, finish() is
                // a no-op and the error propagates through the output streams
                // so the gRPC body sees it.
                spawnCont.finish(throwing: error)
                stdoutCont.finish(throwing: error)
                stderrCont.finish(throwing: error)
            }

            await self?.handleAppTermination(id: appName, launchToken: launchToken)
        }

        for try await pid in spawnStream {
            return AttachedDockerRun(
                pid: pid,
                runTask: runTask,
                stdout: stdoutStream,
                stderr: stderrStream
            )
        }

        // Subprocess.run exited without ever entering the body closure and
        // without throwing. This shouldn't happen in practice.
        throw RPCError(
            code: .internalError,
            message: "docker attach never confirmed start"
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

        let isLinux = appConfig?.platform?.hasPrefix("linux") == true

        if isLinux {
            // Docker path for Linux containers.
            guard self.dockerPresent else {
                throw RPCError(
                    code: .failedPrecondition,
                    message:
                        "Docker is required for Linux containers but was not found. Install Docker Desktop, Colima, or OrbStack."
                )
            }

            // Pull the image from the local registry into Docker.
            logger.info("Pulling image", metadata: ["image": "\(imageName)"])
            _ = try await self.runDocker(["pull", imageName])
            logger.info(
                "Linux container image pulled via Docker",
                metadata: ["app_name": "\(appName)"]
            )
            try await self.registerApp(
                id: appName,
                kind: .container,
                container: WendyApp.ContainerMetadata(
                    imageName: imageName,
                    appConfig: appConfig
                )
            )
            return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
        }

        // Native darwin path (existing behavior).

        let nativeLaunchInfo: NativeLaunchInfo
        if imageName.hasPrefix("sha256:") {
            // OCI image: parse manifest → config → extract layer.
            let appDirectory = appsBase.appendingPathComponent(appName).path
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

            let binaryPath = "\(appDirectory)/\(binaryName)"
            guard FileManager.default.fileExists(atPath: binaryPath) else {
                throw RPCError(
                    code: .notFound,
                    message: "Binary not found at \(binaryPath) after extraction"
                )
            }

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
            let appDirectory = appsBase.appendingPathComponent(appName).path
            let binaryPath = "\(appDirectory)/\(imageName)"
            guard FileManager.default.fileExists(atPath: binaryPath) else {
                throw RPCError(code: .notFound, message: "Binary not found at \(binaryPath)")
            }
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
            let appDirectory = appsBase.appendingPathComponent(appName).path
            let binaryPath = "\(appDirectory)/\(cmd)"

            guard FileManager.default.fileExists(atPath: binaryPath) else {
                throw RPCError(
                    code: .notFound,
                    message:
                        "Binary not found at \(binaryPath). Run 'wendy run' to sync files first."
                )
            }

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

        try await self.registerApp(id: appName, kind: .native, native: nativeLaunchInfo)
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

        guard let app = self.appsByID[appName] else {
            throw RPCError(
                code: .failedPrecondition,
                message: "No registered app found for \(appName). Call CreateContainer first."
            )
        }

        // Docker path for Linux containers.
        if let containerMetadata = app.container, self.dockerPresent {
            let appConfig = containerMetadata.appConfig
            let deviceImage = containerMetadata.imageName
            let containerName = Self.containerName(forAppID: appName)

            // Remove any stale container with the same name before launching.
            _ = try? await self.runDocker(["rm", "-f", containerName])

            var dockerArgs: [String] = [
                "run",
                "--rm",
                "--name", containerName,
                "--label", "wendy.managed=true",
                "--label", "wendy.app-name=\(appName)",
            ]
            if let entitlements = appConfig?.entitlements {
                dockerArgs += try Self.dockerRunArgs(
                    from: entitlements,
                    appName: appName
                )
            }
            dockerArgs.append(deviceImage)

            let launchToken = UUID()
            self.prepareAppForLaunch(id: appName, launchToken: launchToken)

            logger.info(
                "Starting Docker container",
                metadata: [
                    "container": "\(containerName)",
                    "image": "\(deviceImage)",
                ]
            )

            // Launch `docker run` attached via swift-subprocess. The spawn
            // handshake below ensures the app is marked `running` with the
            // real PID before `startContainer` returns, so downstream callers
            // (stop/delete/createContainer) can observe the running state.
            let spawn: AttachedDockerRun
            do {
                spawn = try await self.launchAttachedDockerRun(
                    args: dockerArgs,
                    appName: appName,
                    launchToken: launchToken
                )
            } catch {
                self.cancelAppLaunch(id: appName, launchToken: launchToken)
                throw RPCError(
                    code: .internalError,
                    message: "Failed to launch docker container: \(error)"
                )
            }

            try await self.markAppRunning(
                id: appName,
                pid: spawn.pid,
                dockerRunTask: spawn.runTask,
                launchToken: launchToken
            )
            logger.info(
                "Docker container started",
                metadata: ["app_name": "\(appName)", "pid": "\(spawn.pid)"]
            )

            let broadcaster = self.broadcaster
            let stdoutStream = spawn.stdout
            let stderrStream = spawn.stderr
            let runTask = spawn.runTask
            return StreamingServerResponse { writer in
                var started = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                started.responseType = .started(
                    Wendy_Agent_Services_V1_RunContainerLayersResponse.Started()
                )
                try await writer.write(started)

                try await withThrowingTaskGroup(of: Void.self) { group in
                    group.addTask {
                        for try await data in stdoutStream {
                            var response = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                            response.responseType = .stdoutOutput(.with { $0.data = data })
                            try await writer.write(response)

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
                        for try await data in stderrStream {
                            var response = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                            response.responseType = .stderrOutput(.with { $0.data = data })
                            try await writer.write(response)

                            await Self.broadcastLog(
                                broadcaster: broadcaster,
                                appName: appName,
                                text: String(decoding: data, as: UTF8.self),
                                stream: "stderr",
                                severity: .warn
                            )
                        }
                    }
                    group.addTask { await runTask.value }
                    try await group.waitForAll()
                }
                return Metadata()
            }
        }

        // Native darwin path.

        // Resolve binary path: prefer uploaded app, fall back to --appPath.
        let binaryPath: String
        let profilePath: String?
        let processArgs: [String]
        let currentDirectory: String?
        if let entry = app.native {
            binaryPath = "\(entry.directory)/\(entry.binaryName)"
            let candidateProfile = "\(entry.directory)/sandbox.sb"
            profilePath =
                FileManager.default.fileExists(atPath: candidateProfile) ? candidateProfile : nil
            processArgs = entry.args
            currentDirectory = entry.currentDirectory
        } else {
            binaryPath = executablePath
            profilePath = sandboxProfilePath
            processArgs = []
            currentDirectory = nil
        }

        let process = Foundation.Process()
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
        try await self.markAppRunning(
            id: appName,
            pid: process.processIdentifier,
            nativeProcess: process,
            launchToken: launchToken
        )
        logger.info(
            "Process started",
            metadata: ["app_name": "\(appName)", "pid": "\(process.processIdentifier)"]
        )

        // Capture values for the sendable closure.
        let broadcaster = self.broadcaster

        return StreamingServerResponse { writer in
            // Send "started" message.
            var started = Wendy_Agent_Services_V1_RunContainerLayersResponse()
            started.responseType = .started(
                Wendy_Agent_Services_V1_RunContainerLayersResponse.Started()
            )
            try await writer.write(started)

            try await withThrowingTaskGroup(of: Void.self) { group in
                // Stream stdout.
                group.addTask {
                    let handle = stdoutPipe.fileHandleForReading
                    for try await data in handle.bytes(for: appName) {
                        var response = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                        response.responseType = .stdoutOutput(.with { $0.data = data })
                        try await writer.write(response)

                        await Self.broadcastLog(
                            broadcaster: broadcaster,
                            appName: appName,
                            text: String(decoding: data, as: UTF8.self),
                            stream: "stdout",
                            severity: .info
                        )
                    }
                }

                // Stream stderr.
                group.addTask {
                    let handle = stderrPipe.fileHandleForReading
                    for try await data in handle.bytes(for: appName) {
                        var response = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                        response.responseType = .stderrOutput(.with { $0.data = data })
                        try await writer.write(response)

                        await Self.broadcastLog(
                            broadcaster: broadcaster,
                            appName: appName,
                            text: String(decoding: data, as: UTF8.self),
                            stream: "stderr",
                            severity: .warn
                        )
                    }
                }

                // Wait for process exit.
                group.addTask {
                    process.waitUntilExit()
                }

                try await group.waitForAll()
            }

            return Metadata()
        }
    }

    func stopContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StopContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_StopContainerResponse> {
        let appName = request.message.appName
        logger.info("StopContainer called", metadata: ["app_name": "\(appName)"])

        let didStop = try await self.stopTrackedAppIfRunning(id: appName)
        if didStop {
            if self.appsByID[appName]?.info.kind == .container {
                logger.info("Docker container stopped", metadata: ["app_name": "\(appName)"])
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

        if self.appsByID[appName]?.container != nil, self.dockerPresent {
            _ = try? await self.runDocker(
                ["rm", "-f", Self.containerName(forAppID: appName)]
            )
            await self.removeApp(id: appName)
            logger.info("Docker container removed", metadata: ["app_name": "\(appName)"])
        } else {
            await self.removeApp(id: appName)
        }

        return ServerResponse(message: Wendy_Agent_Services_V1_DeleteContainerResponse())
    }

    func listContainers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListContainersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_ListContainersResponse> {
        let apps = self.currentAppInfos()
        return StreamingServerResponse { writer in
            for app in apps {
                var container = AppContainer()
                container.appName = app.id
                container.runningState = app.status == .running ? .running : .stopped

                var response = Wendy_Agent_Services_V1_ListContainersResponse()
                response.container = container
                try await writer.write(response)
            }

            return Metadata()
        }
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
            return stats
        }
        return ServerResponse(message: response)
    }

    // MARK: - Unimplemented

    func attachContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_AttachContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        throw RPCError(code: .unimplemented, message: "AttachContainer is not implemented")
    }

    func listVolumes(
        request: ServerRequest<Wendy_Agent_Services_V1_ListVolumesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListVolumesResponse> {
        throw RPCError(code: .unimplemented, message: "ListVolumes is not implemented")
    }

    func removeVolume(
        request: ServerRequest<Wendy_Agent_Services_V1_RemoveVolumeRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_RemoveVolumeResponse> {
        throw RPCError(code: .unimplemented, message: "RemoveVolume is not implemented")
    }

    func listLayers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_LayerHeader> {
        throw RPCError(code: .unimplemented, message: "ListLayers is not implemented")
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

            let blobPath = "\(blobsDirectory)/sha256/\(expectedHash)"
            try accumulated.write(to: URL(fileURLWithPath: blobPath))

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

            let appDirectory = appsBase.appendingPathComponent(appName).path
            try FileManager.default.createDirectory(
                atPath: appDirectory,
                withIntermediateDirectories: true
            )
            let filePath = "\(appDirectory)/\(filename)"
            try accumulated.write(to: URL(fileURLWithPath: filePath))

            if filename != "sandbox.sb" {
                try FileManager.default.setAttributes(
                    [.posixPermissions: 0o755],
                    ofItemAtPath: filePath
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
            message: "CreateContainerWithProgress is not implemented"
        )
    }

    func runContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_RunContainerLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        throw RPCError(code: .unimplemented, message: "RunContainer is not implemented")
    }

    // MARK: - Helpers

    private func readBlob(digest: String) throws -> Data {
        // digest is "sha256:<hex>" — map to blobsDirectory/sha256/<hex>.
        let blobPath = "\(blobsDirectory)/\(digest.replacingOccurrences(of: ":", with: "/"))"
        guard let data = FileManager.default.contents(atPath: blobPath) else {
            throw RPCError(code: .notFound, message: "Blob not found at \(blobPath)")
        }
        return data
    }

    private func extractTarGz(blobDigest: String, to destinationDirectory: String) async throws {
        let blobPath = "\(blobsDirectory)/\(blobDigest.replacingOccurrences(of: ":", with: "/"))"
        let tarProcess = Foundation.Process()
        tarProcess.executableURL = URL(fileURLWithPath: "/usr/bin/tar")
        tarProcess.arguments = ["-xzf", blobPath, "-C", destinationDirectory]

        let status = try await withCheckedThrowingContinuation {
            (continuation: CheckedContinuation<Int32, Error>) in
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

        await broadcaster.broadcastLogs(
            Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest.with {
                $0.resourceLogs = [resourceLogs]
            }
        )
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
