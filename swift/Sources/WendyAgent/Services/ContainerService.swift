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
    private struct NativeLaunchInfo {
        let directory: String
        let launchPath: String
        let args: [String]
        let currentDirectory: String?
    }

    private let executablePath: String
    private let logger = Logger(label: "sh.wendy.agent.container")
    private var runningProcesses: [String: Foundation.Process] = [:]
    private let sandboxProfilePath: String?

    /// Maps app name → native launch metadata for apps uploaded via file sync or layers.
    private var appDirectories: [String: NativeLaunchInfo] = [:]

    /// Docker backend for Linux containers. Nil when Docker is not available.
    private let dockerBackend: DockerContainerBackend?

    /// Tracks which apps were created via the Docker backend.
    private var dockerApps: Set<String> = []

    /// Docker image names, keyed by app name. Stored during createContainer so
    /// startContainer uses the exact image that was pulled.
    private var dockerImageNames: [String: String] = [:]

    /// Parsed app configs, keyed by app name. Stored during createContainer for
    /// use by startContainer.
    private var appConfigs: [String: WendyAppConfig] = [:]

    init(
        broadcaster: TelemetryBroadcaster,
        executablePath: String,
        sandboxProfilePath: String? = nil,
        appsBase: URL? = nil,
        dockerAvailable: Bool = false
    ) {
        self.broadcaster = broadcaster
        self.executablePath = executablePath
        self.sandboxProfilePath = sandboxProfilePath
        self.dockerBackend = dockerAvailable ? DockerContainerBackend() : nil

        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let baseDirectory = "\(home)/Library/Application Support/wendy-agent"

        self.appsBase = appsBase ?? URL(fileURLWithPath: "\(baseDirectory)/apps")
        self.blobsDirectory = "\(baseDirectory)/blobs"

        // Ensure directories exist.
        try? FileManager.default.createDirectory(
            at: self.appsBase,
            withIntermediateDirectories: true
        )
        try? FileManager.default.createDirectory(
            atPath: "\(blobsDirectory)/sha256",
            withIntermediateDirectories: true
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

        // Parse app config to determine the target platform.
        let appConfig: WendyAppConfig? = {
            let data = request.message.appConfig
            guard !data.isEmpty else { return nil }
            return try? JSONDecoder().decode(WendyAppConfig.self, from: data)
        }()

        let isLinux = appConfig?.platform?.hasPrefix("linux") == true

        if isLinux {
            // Docker backend path for Linux containers.
            guard let dockerBackend else {
                throw RPCError(
                    code: .failedPrecondition,
                    message:
                        "Docker is required for Linux containers but was not found. Install Docker Desktop, Colima, or OrbStack."
                )
            }

            // Pull the image from the local registry into Docker.
            try await dockerBackend.pullImage(imageName)
            dockerApps.insert(appName)
            dockerImageNames[appName] = imageName
            if let appConfig {
                appConfigs[appName] = appConfig
            }
            logger.info(
                "Linux container image pulled via Docker",
                metadata: ["app_name": "\(appName)"]
            )
            return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
        }

        // Native darwin path (existing behavior).

        // Stop any existing process before re-deploying.
        if let existing = runningProcesses.removeValue(forKey: appName) {
            if existing.isRunning {
                existing.terminate()
                existing.waitUntilExit()
            }
            logger.info(
                "Stopped existing process for re-deploy",
                metadata: ["app_name": "\(appName)"]
            )
        }

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

            appDirectories[appName] = NativeLaunchInfo(
                directory: appDirectory,
                launchPath: binaryName,
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
            appDirectories[appName] = NativeLaunchInfo(
                directory: appDirectory,
                launchPath: imageName,
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

            appDirectories[appName] = NativeLaunchInfo(
                directory: appDirectory,
                launchPath: cmd,
                args: Array(request.message.userArgs),
                currentDirectory: appDirectory
            )

            logger.info(
                "Registered app (file-sync path)",
                metadata: ["app_name": "\(appName)", "binary": "\(binaryPath)"]
            )
        }

        return ServerResponse(message: Wendy_Agent_Services_V1_CreateContainerResponse())
    }

    func startContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StartContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        let appName = request.message.appName
        logger.info("StartContainer called", metadata: ["app_name": "\(appName)"])

        // Docker backend path for Linux containers.
        if dockerApps.contains(appName), let dockerBackend {
            let appConfig = appConfigs[appName]
            guard let deviceImage = dockerImageNames[appName] else {
                throw RPCError(
                    code: .failedPrecondition,
                    message: "No Docker image found for \(appName). Call CreateContainer first."
                )
            }

            let (process, stdoutPipe, stderrPipe) = try await dockerBackend.createAndStart(
                appName: appName,
                imageName: deviceImage,
                appConfig: appConfig
            )
            runningProcesses[appName] = process
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

        // Stop any existing process with the same name.
        if let existing = runningProcesses[appName] {
            if existing.isRunning {
                existing.terminate()
                existing.waitUntilExit()
            }
            runningProcesses.removeValue(forKey: appName)
        }

        // Resolve binary path: prefer uploaded app, fall back to --appPath.
        let binaryPath: String
        let profilePath: String?
        let processArgs: [String]
        let currentDirectory: String?
        if let entry = appDirectories[appName] {
            binaryPath = try resolveNativeExecutable(for: entry)
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

        do {
            try process.run()
        } catch {
            throw RPCError(
                code: .internalError,
                message: "Failed to launch process at \(binaryPath): \(error)"
            )
        }
        runningProcesses[appName] = process
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

        if dockerApps.contains(appName), let dockerBackend {
            // Also remove from runningProcesses (the docker run process).
            runningProcesses.removeValue(forKey: appName)
            try await dockerBackend.stop(appName: appName)
            logger.info("Docker container stopped", metadata: ["app_name": "\(appName)"])
        } else if let process = runningProcesses.removeValue(forKey: appName) {
            if process.isRunning {
                process.terminate()
                process.waitUntilExit()
            }
            logger.info("Process stopped", metadata: ["app_name": "\(appName)"])
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

        if dockerApps.contains(appName), let dockerBackend {
            runningProcesses.removeValue(forKey: appName)
            try await dockerBackend.remove(appName: appName)
            dockerApps.remove(appName)
            dockerImageNames.removeValue(forKey: appName)
            appConfigs.removeValue(forKey: appName)
            logger.info("Docker container removed", metadata: ["app_name": "\(appName)"])
        } else {
            // Stop if running, then remove.
            if let process = runningProcesses.removeValue(forKey: appName) {
                if process.isRunning {
                    process.terminate()
                    process.waitUntilExit()
                }
            }
        }

        return ServerResponse(message: Wendy_Agent_Services_V1_DeleteContainerResponse())
    }

    func listContainers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListContainersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_ListContainersResponse> {
        // Collect native processes.
        let processes = runningProcesses

        // Collect Docker containers.
        let dockerContainers: [DockerCLI.ContainerInfo]
        if let dockerBackend {
            dockerContainers = (try? await dockerBackend.listContainers()) ?? []
        } else {
            dockerContainers = []
        }

        let dockerAppNames = dockerApps
        return StreamingServerResponse { writer in
            // Native processes.
            for (appName, process) in processes where !dockerAppNames.contains(appName) {
                var container = AppContainer()
                container.appName = appName
                container.runningState = process.isRunning ? .running : .stopped

                var response = Wendy_Agent_Services_V1_ListContainersResponse()
                response.container = container
                try await writer.write(response)
            }

            // Docker containers.
            for info in dockerContainers {
                var container = AppContainer()
                // Strip "wendy-" prefix from container name.
                let name = info.names
                container.appName = name.hasPrefix("wendy-") ? String(name.dropFirst(6)) : name
                container.runningState = info.state == "running" ? .running : .stopped

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
        let appNames = Set(appDirectories.keys)
            .union(runningProcesses.keys)
            .union(dockerApps)
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

    private func resolveNativeExecutable(for entry: NativeLaunchInfo) throws -> String {
        let launchURL = URL(fileURLWithPath: entry.directory).appendingPathComponent(entry.launchPath)
        if launchURL.pathExtension == "app" {
            return try resolveAppBundleExecutable(at: launchURL)
        }
        return launchURL.path
    }

    private func resolveAppBundleExecutable(at bundleURL: URL) throws -> String {
        let contentsURL = bundleURL.appendingPathComponent("Contents", isDirectory: true)
        let macOSURL = contentsURL.appendingPathComponent("MacOS", isDirectory: true)

        if let infoPlistExecutable = try loadCFBundleExecutable(from: contentsURL) {
            let candidate = macOSURL.appendingPathComponent(infoPlistExecutable)
            if FileManager.default.isExecutableFile(atPath: candidate.path) {
                return candidate.path
            }
        }

        let bundleStem = bundleURL.deletingPathExtension().lastPathComponent
        let stemCandidate = macOSURL.appendingPathComponent(bundleStem)
        if FileManager.default.isExecutableFile(atPath: stemCandidate.path) {
            return stemCandidate.path
        }

        let entries: [URL]
        do {
            entries = try FileManager.default.contentsOfDirectory(
                at: macOSURL,
                includingPropertiesForKeys: [.isRegularFileKey],
                options: [.skipsHiddenFiles]
            )
        } catch {
            throw RPCError(
                code: .notFound,
                message: "App bundle is missing Contents/MacOS: \(bundleURL.lastPathComponent)"
            )
        }

        let executableCandidates = entries.filter { candidate in
            FileManager.default.isExecutableFile(atPath: candidate.path)
        }

        if executableCandidates.count == 1, let only = executableCandidates.first {
            return only.path
        }

        if executableCandidates.count > 1 {
            let names = executableCandidates.map(\.lastPathComponent).sorted().joined(separator: ", ")
            throw RPCError(
                code: .invalidArgument,
                message: "App bundle \(bundleURL.lastPathComponent) contains multiple plausible executables in Contents/MacOS: \(names)"
            )
        }

        throw RPCError(
            code: .notFound,
            message: "Could not resolve app bundle executable for \(bundleURL.lastPathComponent)"
        )
    }

    private func loadCFBundleExecutable(from contentsURL: URL) throws -> String? {
        let infoPlistURL = contentsURL.appendingPathComponent("Info.plist")
        guard FileManager.default.fileExists(atPath: infoPlistURL.path) else {
            return nil
        }

        let data = try Data(contentsOf: infoPlistURL)
        let plist = try PropertyListSerialization.propertyList(from: data, format: nil)
        guard let dictionary = plist as? [String: Any] else {
            return nil
        }
        return dictionary["CFBundleExecutable"] as? String
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
