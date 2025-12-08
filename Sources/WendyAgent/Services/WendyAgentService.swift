import Crypto
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import WendyAgentGRPC
import WendyShared
import _NIOFileSystem

/// Actor to coordinate concurrent update requests
actor UpdateCoordinator {
    private var isUpdating = false

    func acquireUpdateLock() throws {
        if isUpdating {
            throw RPCError(
                code: .alreadyExists,
                message: "Another update is already in progress"
            )
        }
        isUpdating = true
    }

    func releaseUpdateLock() {
        isUpdating = false
    }
}

struct WendyAgentService: Wendy_Agent_Services_V1_WendyAgentService.ServiceProtocol {
    let logger = Logger(label: "WendyAgentService")
    let shouldRestart: @Sendable () async throws -> Void
    let currentUID: String
    let networkManagerFactory: NetworkConnectionManagerFactory
    let configuration: WendyAgentConfiguration
    let updateCoordinator = UpdateCoordinator()

    init(shouldRestart: @escaping @Sendable () async throws -> Void) {
        self.shouldRestart = shouldRestart
        self.currentUID = String(getuid())
        self.networkManagerFactory = NetworkConnectionManagerFactory(uid: currentUID)
        self.configuration = WendyAgentConfiguration.fromEnvironment()
    }

    /// Helper to set executable permissions on a file with proper error handling
    private func setExecutablePermissions(
        path: FilePath,
        permissions: UInt16,
        fileManager: FileManager
    ) throws {
        do {
            try fileManager.setAttributes(
                [.posixPermissions: NSNumber(value: permissions)],
                ofItemAtPath: path.string
            )
            logger.debug(
                "Set permissions on file",
                metadata: [
                    "path": "\(path)",
                    "permissions": "\(String(format: "%o", permissions))",
                ]
            )
        } catch {
            logger.error(
                "Failed to set permissions on file",
                metadata: [
                    "path": "\(path)",
                    "permissions": "\(String(format: "%o", permissions))",
                    "error": "\(error)",
                ]
            )
            throw RPCError(
                code: .internalError,
                message: "Failed to set executable permissions on \(path): \(error)"
            )
        }
    }

    func runContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_RunContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerResponse> {
        throw RPCError(
            code: .unavailable,
            message: "Use the newer WendyContainerService APIs instead"
        )
    }

    func updateAgent(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateAgentResponse> {
        logger.info("Updating agent")
        return StreamingServerResponse { writer in
            // Acquire exclusive lock to prevent concurrent updates
            try await updateCoordinator.acquireUpdateLock()

            // Perform update and ensure lock is always released
            do {
                try await performUpdate(request: request, writer: writer)
            } catch {
                await updateCoordinator.releaseUpdateLock()
                throw error
            }

            // Lock will be released after restart succeeds (process exits)
            // or in the error handler above if update fails
            return Metadata()
        }
    }

    private func performUpdate(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest>,
        writer: GRPCCore.RPCWriter<Wendy_Agent_Services_V1_UpdateAgentResponse>
    ) async throws {
        // Resolve symlinks to get the actual binary location
        let rawBinaryPath = FilePath(ProcessInfo.processInfo.arguments[0])
        let currentBinary = resolveSymlinks(rawBinaryPath)
        let filesystem = FileSystem.shared

        logger.info(
            "Acquired update lock, checking current binary at \(currentBinary) (resolved from \(rawBinaryPath))"
        )
        guard
            let info = try await filesystem.info(forFileAt: currentBinary),
            info.type == .regular
        else {
            logger.error("Current binary is not a regular file")
            throw RPCError(
                code: .invalidArgument,
                message: "Invalid request: Current binary is not a regular file"
            )
        }

        // Capture permissions immediately after verifying file exists
        let fileManager = FileManager.default
        let originalAttributes = try fileManager.attributesOfItem(atPath: currentBinary.string)
        guard let permissionsNumber = originalAttributes[.posixPermissions] as? NSNumber else {
            logger.error("Failed to capture file permissions")
            throw RPCError(
                code: .internalError,
                message: "Failed to capture executable permissions from current binary"
            )
        }
        let originalPermissions = permissionsNumber.uint16Value

        logger.info("Creating temporary directory")
        let tempDir = try await filesystem.createTemporaryDirectory(
            template: "/tmp/wendy-agent-update-XXX"
        )
        let updateFile = tempDir.appending("wendy-agent")

        // Ensure temp directory is cleaned up if update fails
        var shouldCleanupTemp = true
        defer {
            if shouldCleanupTemp {
                Task {
                    do {
                        try await filesystem.removeItem(at: tempDir)
                    } catch {
                        logger.warning(
                            "Failed to clean up temporary directory",
                            metadata: [
                                "path": "\(tempDir)",
                                "error": "\(error)",
                            ]
                        )
                    }
                }
            }
        }

        logger.info("Writing update to \(updateFile)")
        try await filesystem.withFileHandle(
            forReadingAndWritingAt: updateFile,
            options: .newFile(
                replaceExisting: true,
                permissions: [.ownerReadWriteExecute, .groupReadExecute, .otherReadExecute]
            )
        ) { writer in
            var bufferedWriter = writer.bufferedWriter()
            var hash = SHA256()
            for try await event in request.messages {
                switch event.requestType {
                case .chunk(let chunk):
                    hash.update(data: chunk.data)
                    try await bufferedWriter.write(contentsOf: ByteBuffer(data: chunk.data))
                case .control(let update):
                    let finalHash = hash.finalize().map { String(format: "%02x", $0) }.joined()
                    guard
                        update.update.sha256.isEmpty  // If the hash is empty, we don't check it
                            || finalHash.caseInsensitiveCompare(update.update.sha256)
                                == .orderedSame
                    else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Invalid request: SHA256 hash mismatch"
                        )
                    }
                    try await bufferedWriter.flush()
                    logger.info("Received control command, binary is written")
                    return
                case .none:
                    // Unknown, ignore.
                    ()
                }
                try await bufferedWriter.flush()
            }
        }

        logger.info("Applying update to \(currentBinary)")

        // Create backup of current binary before replacement
        guard let binaryName = currentBinary.lastComponent?.string else {
            throw RPCError(
                code: .internalError,
                message: "Failed to get binary name from \(currentBinary)"
            )
        }
        let backupFile =
            currentBinary
            .removingLastComponent()
            .appending(binaryName + ".backup")
        logger.info("Creating backup at \(backupFile)")

        // Remove any existing backup to ensure clean state
        if try await filesystem.info(forFileAt: backupFile) != nil {
            try await filesystem.removeItem(at: backupFile)
        }

        // Copy current binary to backup (not move, to keep original during replacement)
        try await filesystem.copyItem(at: currentBinary, to: backupFile)

        // Explicitly preserve executable permissions on backup
        try setExecutablePermissions(
            path: backupFile,
            permissions: originalPermissions,
            fileManager: fileManager
        )

        // Perform atomic replacement with error recovery
        do {
            // Use atomic rename by moving new binary to temp location first,
            // then renaming it to replace the current binary.
            // On POSIX systems, rename() is atomic when source and dest are on same filesystem.
            let tempNewBinary = currentBinary.appending(".new")

            // First, move update file to temp location next to target
            // This ensures we're on the same filesystem for atomic rename
            try await filesystem.moveItem(at: updateFile, to: tempNewBinary)

            // Temp file has been moved, no longer need to clean up temp directory
            shouldCleanupTemp = false

            // Now atomically replace the current binary with the new one
            // This operation is atomic - old file disappears and new file appears simultaneously
            try await filesystem.moveItem(at: tempNewBinary, to: currentBinary)

            // Ensure the new binary has executable permissions
            try setExecutablePermissions(
                path: currentBinary,
                permissions: originalPermissions,
                fileManager: fileManager
            )

            logger.info("Update applied successfully, backup kept at \(backupFile)")
            logger.info("Restarting agent")

            // Attempt restart - if this throws, we'll restore from backup
            try await shouldRestart()

            // Note: If restart succeeds, this code won't execute as the process will exit
            // The backup file will remain and should be cleaned up on next successful start
        } catch {
            // If move failed or restart failed, restore from backup
            logger.error("Update failed: \(error), attempting to restore from backup")

            // Clean up temp files
            let tempNewBinary = currentBinary.appending(".new")
            if (try? await filesystem.info(forFileAt: tempNewBinary)) != nil {
                _ = try? await filesystem.removeItem(at: tempNewBinary)
            }

            // Remove the potentially corrupt/incomplete new binary if it exists
            if (try? await filesystem.info(forFileAt: currentBinary)) != nil {
                _ = try? await filesystem.removeItem(at: currentBinary)
            }

            // Restore from backup
            do {
                try await filesystem.moveItem(at: backupFile, to: currentBinary)

                // Ensure restored binary has executable permissions
                try setExecutablePermissions(
                    path: currentBinary,
                    permissions: originalPermissions,
                    fileManager: fileManager
                )

                logger.info("Successfully restored from backup with original permissions")
            } catch {
                logger.critical(
                    "Failed to restore from backup: \(error). System may be in inconsistent state."
                )
            }

            // Re-throw the original error
            throw RPCError(
                code: .internalError,
                message: "Update failed: \(error.localizedDescription). Restored from backup."
            )
        }

        try await writer.write(
            .with {
                $0.updated = .init()
            }
        )
    }

    func getAgentVersion(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_GetAgentVersionRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_GetAgentVersionResponse> {
        return ServerResponse(
            message: .with {
                $0.version = Version.current
            }
        )
    }

    func listWiFiNetworks(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_ListWiFiNetworksRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_ListWiFiNetworksResponse> {
        logger.info("Listing available WiFi networks")

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            let wifiNetworks = try await networkManager.listWiFiNetworks()

            logger.info("Found \(wifiNetworks.count) WiFi networks")

            return ServerResponse(
                message: .with { response in
                    response.networks = wifiNetworks.map { network in
                        .with { protoNetwork in
                            protoNetwork.ssid = network.ssid
                            // Include signal strength if available
                            if let signalStrength = network.signalStrength {
                                protoNetwork.signalStrength = Int32(signalStrength)
                            }
                        }
                    }
                }
            )
        } catch {
            logger.error("Failed to list WiFi networks: \(error)")
            throw RPCError(
                code: .internalError,
                message: "Failed to list WiFi networks: \(error.localizedDescription)"
            )
        }
    }

    func connectToWiFi(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_ConnectToWiFiRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_ConnectToWiFiResponse> {
        let ssid = request.message.ssid
        let password = request.message.password

        logger.info("Connecting to WiFi network", metadata: ["ssid": "\(ssid)"])

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            try await networkManager.setupWiFi(ssid: ssid, password: password)

            if let connection = try await networkManager.getCurrentConnection() {
                logger.info(
                    "Successfully connected to WiFi network",
                    metadata: ["ssid": "\(connection.ssid)"]
                )
                return ServerResponse(
                    message: .with { $0.success = true }
                )
            } else {
                logger.warning("Failed to connect to WiFi network", metadata: ["ssid": "\(ssid)"])
                return ServerResponse(
                    message: .with {
                        $0.success = false
                        $0.errorMessage = "Connection failed"
                    }
                )
            }
        } catch {
            logger.error(
                "Error connecting to WiFi network",
                metadata: [
                    "ssid": "\(ssid)",
                    "error": "\(error)",
                ]
            )

            return ServerResponse(
                message: .with {
                    $0.success = false
                    $0.errorMessage = error.localizedDescription
                }
            )
        }
    }

    func getWiFiStatus(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_GetWiFiStatusRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_GetWiFiStatusResponse> {
        logger.info("Getting WiFi connection status")

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )
            let connectionInfo = try await networkManager.getCurrentConnection()

            if let connectionInfo = connectionInfo {
                logger.info(
                    "Currently connected to WiFi network",
                    metadata: ["ssid": "\(connectionInfo.ssid)"]
                )
                return ServerResponse(
                    message: .with { response in
                        response.connected = true
                        response.ssid = connectionInfo.ssid
                    }
                )
            } else {
                logger.info("Not connected to any WiFi network")
                return ServerResponse(
                    message: .with { response in
                        response.connected = false
                    }
                )
            }
        } catch {
            logger.error("Error getting WiFi status: \(error)")

            return ServerResponse(
                message: .with { response in
                    response.connected = false
                    response.errorMessage = error.localizedDescription
                }
            )
        }
    }

    func disconnectWiFi(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_DisconnectWiFiRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_DisconnectWiFiResponse> {
        logger.info("Disconnecting from WiFi network")

        do {
            let networkManager = try await networkManagerFactory.createNetworkManager(
                preference: configuration.networkManagerPreference
            )

            // Check current connection first
            if let connectionInfo = try await networkManager.getCurrentConnection() {
                logger.info(
                    "Disconnecting from WiFi network",
                    metadata: ["ssid": "\(connectionInfo.ssid)"]
                )

                let success = try await networkManager.disconnectFromNetwork()

                if success {
                    logger.info(
                        "Successfully disconnected from WiFi network",
                        metadata: ["ssid": "\(connectionInfo.ssid)"]
                    )
                    return ServerResponse(
                        message: .with { $0.success = true }
                    )
                } else {
                    logger.warning(
                        "Failed to disconnect from WiFi network",
                        metadata: ["ssid": "\(connectionInfo.ssid)"]
                    )
                    return ServerResponse(
                        message: .with {
                            $0.success = false
                            $0.errorMessage = "Disconnection failed"
                        }
                    )
                }
            } else {
                logger.info("Not connected to any WiFi network")
                return ServerResponse(
                    message: .with {
                        $0.success = true
                    }
                )
            }
        } catch NetworkConnectionError.noActiveConnection {
            // Not being connected is not an error for disconnection
            logger.info("Not connected to any WiFi network")
            return ServerResponse(
                message: .with {
                    $0.success = true
                }
            )
        } catch {
            logger.error("Error disconnecting from WiFi network: \(error)")

            return ServerResponse(
                message: .with {
                    $0.success = false
                    $0.errorMessage = error.localizedDescription
                }
            )
        }
    }

    func listHardwareCapabilities(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest>,
        context: GRPCCore.ServerContext
    ) async throws
        -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse>
    {
        logger.info("Listing hardware capabilities")

        do {
            let discoverer = SystemHardwareDiscoverer()
            let capabilities = try await discoverer.discoverCapabilities(
                categoryFilter: request.message.hasCategoryFilter
                    ? request.message.categoryFilter : nil
            )

            logger.info("Found \(capabilities.count) hardware capabilities")

            return ServerResponse(
                message: .with { response in
                    response.capabilities = capabilities.map { $0.toProto() }
                }
            )
        } catch {
            logger.error("Failed to discover hardware capabilities: \(error)")
            throw RPCError(
                code: .internalError,
                message: "Failed to discover hardware capabilities: \(error.localizedDescription)"
            )
        }
    }
}
