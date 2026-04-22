import CryptoKit
import Foundation
import GRPCCore
import Logging
import OpenTelemetryGRPC
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

    /// Docker CLI for Linux containers. Nil when Docker is not available.
    ///
    /// Linux container support on macOS used to live in a separate
    /// `DockerContainerBackend` type, but that type was a thin shim over
    /// `DockerCLI` that only added app-name/container-name and entitlement
    /// mapping. Those concerns are app-lifecycle concerns, so they now live
    /// on `ContainerService` directly and this service talks to `DockerCLI`.
    private let docker: DockerCLI?

    init(
        broadcaster: TelemetryBroadcaster,
        executablePath: String,
        sandboxProfilePath: String? = nil,
        stateDirectory: URL? = nil,
        appsBase: URL? = nil,
        dockerAvailable: Bool = false,
        onAppsChanged: @escaping @Sendable ([WendyAppInfo]) async -> Void = { _ in }
    ) {
        self.broadcaster = broadcaster
        self.onAppsChanged = onAppsChanged
        self.executablePath = executablePath
        self.sandboxProfilePath = sandboxProfilePath
        self.docker = dockerAvailable ? DockerCLI() : nil

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
        app.launchToken = launchToken
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
        guard app.info != stoppedInfo || app.process != nil || app.launchToken != nil else {
            return
        }

        app.info = stoppedInfo
        app.process = nil
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
            let process = app.process,
            let launchToken = app.launchToken,
            app.info.status == .running
        else {
            return false
        }

        if !process.isRunning {
            await self.handleAppTermination(id: id, launchToken: launchToken)
            return true
        }

        let exitTask = Self.makeProcessExitTask(process)

        if app.info.kind == .container, app.container != nil, let docker {
            _ = try? await docker.stop(container: Self.containerName(forAppID: id), timeout: 10)
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

    // MARK: - Docker helpers

    /// Derive the Docker container name for a given Wendy app id.
    nonisolated private static func containerName(forAppID appID: String) -> String {
        "wendy-\(appID)"
    }

    /// Translate Wendy entitlements into `docker run` options.
    ///
    /// Throws an `RPCError` if any entitlement cannot be honored, so callers
    /// fail fast rather than launching a container that silently lacks the
    /// capabilities the app was promised.
    nonisolated private static func dockerOptions(
        from entitlements: [WendyEntitlement],
        appName: String
    ) throws -> [DockerCLI.RunOption] {
        var options: [DockerCLI.RunOption] = []

        for entitlement in entitlements {
            switch entitlement.type {
            case "network":
                if entitlement.mode == "none" {
                    options.append(.network("none"))
                } else if let ports = entitlement.ports {
                    // --network=host doesn't work on Docker Desktop for Mac, so
                    // map explicit ports from the entitlement's ports array.
                    for port in ports {
                        options.append(
                            .publish(hostPort: port.host, containerPort: port.container)
                        )
                    }
                }

            case "persist":
                if let name = entitlement.name, let path = entitlement.path {
                    let volumeName = "wendy-\(appName)-\(name)"
                    options.append(.volume(hostOrName: volumeName, containerPath: path))
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

        return options
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
            guard let docker else {
                throw RPCError(
                    code: .failedPrecondition,
                    message:
                        "Docker is required for Linux containers but was not found. Install Docker Desktop, Colima, or OrbStack."
                )
            }

            // Pull the image from the local registry into Docker.
            logger.info("Pulling image", metadata: ["image": "\(imageName)"])
            try await docker.pull(image: imageName)
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
        if let containerMetadata = app.container, let docker {
            let appConfig = containerMetadata.appConfig
            let deviceImage = containerMetadata.imageName
            let containerName = Self.containerName(forAppID: appName)

            // Remove any stale container with the same name before launching.
            _ = try? await docker.rm(options: [.force], container: containerName)

            var runOptions: [DockerCLI.RunOption] = [
                .rm,
                .name(containerName),
                .label(key: "wendy.managed", value: "true"),
                .label(key: "wendy.app-name", value: appName),
            ]
            if let entitlements = appConfig?.entitlements {
                runOptions += try Self.dockerOptions(
                    from: entitlements,
                    appName: appName
                )
            }

            let launchToken = UUID()
            self.prepareAppForLaunch(id: appName, launchToken: launchToken)

            logger.info(
                "Starting Docker container",
                metadata: [
                    "container": "\(containerName)",
                    "image": "\(deviceImage)",
                ]
            )

            let process: Foundation.Process
            let stdoutPipe: Pipe
            let stderrPipe: Pipe
            do {
                (process, stdoutPipe, stderrPipe) = try docker.runAttached(
                    options: runOptions,
                    image: deviceImage,
                    terminationHandler: self.makeTerminationHandler(
                        forAppID: appName,
                        launchToken: launchToken
                    )
                )
            } catch {
                self.cancelAppLaunch(id: appName, launchToken: launchToken)
                throw error
            }

            try await self.markAppRunning(id: appName, process: process, launchToken: launchToken)
            logger.info(
                "Docker container started",
                metadata: ["app_name": "\(appName)", "pid": "\(process.processIdentifier)"]
            )

            let broadcaster = self.broadcaster
            return StreamingServerResponse { writer in
                var started = Wendy_Agent_Services_V1_RunContainerLayersResponse()
                started.responseType = .started(
                    Wendy_Agent_Services_V1_RunContainerLayersResponse.Started()
                )
                try await writer.write(started)

                try await withThrowingTaskGroup(of: Void.self) { group in
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
                    group.addTask { process.waitUntilExit() }
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
        try await self.markAppRunning(id: appName, process: process, launchToken: launchToken)
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

        if self.appsByID[appName]?.container != nil, let docker {
            _ = try? await docker.rm(
                options: [.force],
                container: Self.containerName(forAppID: appName)
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
