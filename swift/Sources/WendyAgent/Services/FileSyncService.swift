import CryptoKit
import Foundation
import GRPCCore
import Logging
import WendyAgentGRPC

/// FileSyncService implements the WendyFileSyncService gRPC protocol.
/// Each app gets an isolated working directory under `appsBase/<appId>`.
actor FileSyncService: Wendy_Agent_Services_V1_WendyFileSyncService.ServiceProtocol {
    private static let sha256Length = 32

    private struct TransferState {
        let path: String
        let manifestEntry: Wendy_Agent_Services_V1_FileSyncEntry
        let destinationURL: URL
        let temporaryURL: URL
        let fileHandle: FileHandle
        var hasher = SHA256()
        var bytesReceived: Int64 = 0
        var nextExpectedSequence: UInt64 = 0
    }

    private let appsBase: URL
    private let logger = Logger(label: "sh.wendy.agent.filesync")

    /// Default storage root: ~/Library/Application Support/wendy-agent/apps
    init(appsBase: URL? = nil) {
        if let base = appsBase {
            self.appsBase = base
        } else {
            let home = FileManager.default.homeDirectoryForCurrentUser
            self.appsBase =
                home.appendingPathComponent("Library/Application Support/wendy-agent/apps")
        }
    }

    // MARK: - gRPC handler

    func syncFiles(
        request: GRPCCore.StreamingServerRequest<Wendy_Agent_Services_V1_FileSyncRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.StreamingServerResponse<Wendy_Agent_Services_V1_FileSyncResponse> {
        // StreamingServerRequest and its RPCAsyncSequence are both Sendable,
        // so we can capture `messages` directly in the @Sendable response closure.
        let messages = request.messages
        let appsBase = self.appsBase
        let logger = self.logger

        return StreamingServerResponse(metadata: [:]) { writer in
            try await FileSyncService.runSession(
                messages: messages,
                writeResponse: { try await writer.write($0) },
                appsBase: appsBase,
                logger: logger
            )
            return Metadata()
        }
    }

    // MARK: - Session logic (nonisolated so it can be captured in @Sendable closures)

    static func runSession<S: AsyncSequence & Sendable>(
        messages: S,
        writeResponse: (Wendy_Agent_Services_V1_FileSyncResponse) async throws -> Void,
        appsBase: URL,
        logger: Logger
    ) async throws where S.Element == Wendy_Agent_Services_V1_FileSyncRequest {
        var messageIterator = messages.makeAsyncIterator()
        guard let first = try await messageIterator.next(), case .start(let startMsg) = first.requestType else {
            throw RPCError(code: .invalidArgument, message: "First message must be FileSyncStart")
        }

        let appID = startMsg.appID
        let workDir = appsBase.appendingPathComponent(appID)

        // Ensure working directory exists.
        try FileManager.default.createDirectory(at: workDir, withIntermediateDirectories: true)

        let cliManifest = startMsg.manifest.files
        let manifestByPath = try manifestLookup(from: cliManifest, in: workDir)

        // Build agent manifest and send it.
        let agentManifest = try buildManifest(at: workDir)
        var manifestResponse = Wendy_Agent_Services_V1_FileSyncResponse()
        var fileSyncManifest = Wendy_Agent_Services_V1_FileSyncManifest()
        fileSyncManifest.files = agentManifest
        manifestResponse.responseType = .manifest(fileSyncManifest)
        try await writeResponse(manifestResponse)

        logger.info(
            "FileSyncStart",
            metadata: [
                "app_id": "\(appID)",
                "cli_files": "\(cliManifest.count)",
                "agent_files": "\(agentManifest.count)",
            ]
        )

        var finalizedPaths = Set<String>()
        var activeTransfer: TransferState?

        func cleanupActiveTransfer() {
            guard let transfer = activeTransfer else { return }
            try? transfer.fileHandle.close()
            try? FileManager.default.removeItem(at: transfer.temporaryURL)
            activeTransfer = nil
        }

        func manifestEntry(for path: String) throws -> Wendy_Agent_Services_V1_FileSyncEntry {
            guard let entry = manifestByPath[path] else {
                throw RPCError(
                    code: .invalidArgument,
                    message: "No manifest entry for \(path)"
                )
            }
            return entry
        }

        func sendAck(for path: String) async throws {
            var ackResponse = Wendy_Agent_Services_V1_FileSyncResponse()
            var ack = Wendy_Agent_Services_V1_FileSyncAck()
            ack.path = path
            ackResponse.responseType = .ack(ack)
            try await writeResponse(ackResponse)
        }

        do {
            while let message = try await messageIterator.next() {
                switch message.requestType {
                case .chunk(let chunk):
                    let entry = try manifestEntry(for: chunk.path)
                    let destinationURL = try validatedDestination(for: chunk.path, in: workDir)

                    guard !finalizedPaths.contains(chunk.path) else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Path already finalized: \(chunk.path)"
                        )
                    }
                    guard chunk.sha256.count == sha256Length else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Chunk SHA256 must be exactly 32 bytes for \(chunk.path)"
                        )
                    }
                    guard !(chunk.data.isEmpty && entry.size > 0) else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Zero-length chunk is not allowed for non-empty file \(chunk.path)"
                        )
                    }

                    if let transfer = activeTransfer {
                        guard transfer.path == chunk.path else {
                            throw RPCError(
                                code: .invalidArgument,
                                message: "Cannot switch paths mid-transfer from \(transfer.path) to \(chunk.path)"
                            )
                        }
                    } else {
                        let temporaryURL = try temporaryURL(for: destinationURL, digest: entry.sha256)
                        try FileManager.default.createDirectory(
                            at: temporaryURL.deletingLastPathComponent(),
                            withIntermediateDirectories: true
                        )
                        FileManager.default.createFile(atPath: temporaryURL.path, contents: nil)
                        guard let fileHandle = FileHandle(forWritingAtPath: temporaryURL.path) else {
                            throw RPCError(
                                code: .internalError,
                                message: "Cannot open temporary file at \(temporaryURL.path)"
                            )
                        }
                        activeTransfer = TransferState(
                            path: chunk.path,
                            manifestEntry: entry,
                            destinationURL: destinationURL,
                            temporaryURL: temporaryURL,
                            fileHandle: fileHandle
                        )
                    }

                    guard var transfer = activeTransfer else {
                        throw RPCError(code: .internalError, message: "Missing active transfer state")
                    }

                    let isFirstEmptyChunk =
                        transfer.bytesReceived == 0
                        && transfer.manifestEntry.size == 0
                        && transfer.nextExpectedSequence == 0
                    guard transfer.bytesReceived < transfer.manifestEntry.size || isFirstEmptyChunk else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Received extra chunk after reaching declared size for \(chunk.path)"
                        )
                    }
                    guard chunk.sequence == transfer.nextExpectedSequence else {
                        throw RPCError(
                            code: .invalidArgument,
                            message:
                                "Unexpected chunk sequence for \(chunk.path): expected \(transfer.nextExpectedSequence), got \(chunk.sequence)"
                        )
                    }

                    var updatedHasher = transfer.hasher
                    updatedHasher.update(data: chunk.data)
                    let computedSize = transfer.bytesReceived + Int64(chunk.data.count)
                    guard computedSize <= transfer.manifestEntry.size else {
                        throw RPCError(
                            code: .invalidArgument,
                            message:
                                "Chunk for \(chunk.path) exceeds declared size \(transfer.manifestEntry.size)"
                        )
                    }

                    let computedDigest = Data(updatedHasher.finalize())
                    guard computedSize == chunk.cumulativeSize else {
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "Chunk cumulative size mismatch for \(chunk.path): expected \(computedSize), got \(chunk.cumulativeSize)"
                        )
                    }
                    guard computedDigest == chunk.sha256 else {
                        throw RPCError(
                            code: .dataLoss,
                            message: "Chunk SHA256 mismatch for \(chunk.path)"
                        )
                    }

                    try transfer.fileHandle.write(contentsOf: chunk.data)
                    transfer.hasher = updatedHasher
                    transfer.bytesReceived = computedSize
                    transfer.nextExpectedSequence += 1
                    activeTransfer = transfer

                case .commit(let commit):
                    guard let transfer = activeTransfer else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "No active transfer for commit \(commit.path)"
                        )
                    }
                    guard !finalizedPaths.contains(commit.path) else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Path already finalized: \(commit.path)"
                        )
                    }
                    guard transfer.path == commit.path else {
                        throw RPCError(
                            code: .invalidArgument,
                            message:
                                "Commit path \(commit.path) does not match active transfer \(transfer.path)"
                        )
                    }
                    guard commit.sha256.count == sha256Length else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Commit SHA256 must be exactly 32 bytes for \(commit.path)"
                        )
                    }

                    let entry = transfer.manifestEntry
                    guard commit.size == entry.size else {
                        throw RPCError(
                            code: .invalidArgument,
                            message:
                                "Commit size mismatch with manifest for \(commit.path): expected \(entry.size), got \(commit.size)"
                        )
                    }
                    guard commit.sha256 == entry.sha256 else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Commit SHA256 mismatch with manifest for \(commit.path)"
                        )
                    }
                    guard transfer.bytesReceived == entry.size else {
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "Transfer size mismatch for \(commit.path): expected \(entry.size), got \(transfer.bytesReceived)"
                        )
                    }

                    let finalDigest = Data(transfer.hasher.finalize())
                    guard finalDigest == entry.sha256 else {
                        throw RPCError(
                            code: .dataLoss,
                            message: "Transfer SHA256 mismatch for \(commit.path)"
                        )
                    }
                    guard commit.sha256 == finalDigest else {
                        throw RPCError(
                            code: .dataLoss,
                            message: "Commit SHA256 mismatch with transfer state for \(commit.path)"
                        )
                    }

                    try transfer.fileHandle.close()
                    activeTransfer = transfer

                    try FileManager.default.setAttributes(
                        [.posixPermissions: Int(entry.mode)],
                        ofItemAtPath: transfer.temporaryURL.path
                    )
                    try FileManager.default.createDirectory(
                        at: transfer.destinationURL.deletingLastPathComponent(),
                        withIntermediateDirectories: true
                    )
                    if FileManager.default.fileExists(atPath: transfer.destinationURL.path) {
                        try FileManager.default.removeItem(at: transfer.destinationURL)
                    }
                    try FileManager.default.moveItem(
                        at: transfer.temporaryURL,
                        to: transfer.destinationURL
                    )

                    activeTransfer = nil
                    finalizedPaths.insert(commit.path)
                    try await sendAck(for: commit.path)

                    logger.info(
                        "File committed",
                        metadata: ["path": "\(commit.path)", "app_id": "\(appID)"]
                    )

                case .setMode(let setMode):
                    guard activeTransfer == nil else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Cannot apply mode update while a file transfer is active"
                        )
                    }

                    let entry = try manifestEntry(for: setMode.path)
                    let destinationURL = try validatedDestination(for: setMode.path, in: workDir)

                    guard !finalizedPaths.contains(setMode.path) else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Path already finalized: \(setMode.path)"
                        )
                    }
                    guard setMode.sha256.count == sha256Length else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Mode update SHA256 must be exactly 32 bytes for \(setMode.path)"
                        )
                    }
                    guard setMode.size == entry.size else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Mode update size mismatch for \(setMode.path)"
                        )
                    }
                    guard setMode.sha256 == entry.sha256 else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Mode update SHA256 mismatch for \(setMode.path)"
                        )
                    }
                    guard setMode.mode == entry.mode else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "Mode update mode mismatch for \(setMode.path)"
                        )
                    }
                    guard FileManager.default.fileExists(atPath: destinationURL.path) else {
                        throw RPCError(
                            code: .notFound,
                            message: "Cannot apply mode update because \(setMode.path) does not exist"
                        )
                    }

                    try FileManager.default.setAttributes(
                        [.posixPermissions: Int(setMode.mode)],
                        ofItemAtPath: destinationURL.path
                    )
                    finalizedPaths.insert(setMode.path)
                    try await sendAck(for: setMode.path)

                    logger.info(
                        "File mode updated",
                        metadata: ["path": "\(setMode.path)", "app_id": "\(appID)"]
                    )

                case .start, nil:
                    throw RPCError(code: .invalidArgument, message: "Unexpected message in stream")
                }
            }

            if let transfer = activeTransfer {
                throw RPCError(
                    code: .invalidArgument,
                    message: "Missing commit for \(transfer.path)"
                )
            }
        } catch {
            cleanupActiveTransfer()
            throw error
        }

        // Prune stale files: on disk after the session but absent from CLI's declared set.
        let postSessionManifest = try buildManifest(at: workDir)
        let cliPaths = Set(manifestByPath.keys)
        for entry in postSessionManifest where !cliPaths.contains(entry.path) {
            let staleURL = workDir.appendingPathComponent(entry.path)
            try? FileManager.default.removeItem(at: staleURL)
            logger.info(
                "Pruned stale file",
                metadata: ["path": "\(entry.path)", "app_id": "\(appID)"]
            )
        }

        // Send FileSyncComplete.
        var completeResponse = Wendy_Agent_Services_V1_FileSyncResponse()
        completeResponse.responseType = .complete(Wendy_Agent_Services_V1_FileSyncComplete())
        try await writeResponse(completeResponse)
    }

    // MARK: - Path validation

    /// Returns the destination URL for `relativePath` inside `workDir`, throwing if the
    /// path is empty, absolute, contains `.` or `..` components, or resolves outside
    /// `workDir` after symlink expansion.
    static func validatedDestination(for relativePath: String, in workDir: URL) throws -> URL {
        guard !relativePath.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "File path must not be empty")
        }
        guard !relativePath.hasPrefix("/") else {
            throw RPCError(code: .invalidArgument, message: "File path must be relative: \(relativePath)")
        }
        let components = relativePath.split(separator: "/").map(String.init)
        guard !components.contains(".."), !components.contains(".") else {
            throw RPCError(
                code: .invalidArgument,
                message: "File path must not contain . or .. components: \(relativePath)"
            )
        }

        let destination = workDir.appendingPathComponent(relativePath)
        let resolvedWorkDir = workDir.resolvingSymlinksInPath()

        // `resolvingSymlinksInPath()` only resolves components that exist on disk.
        // Walk up from the destination to the deepest ancestor that exists, resolve
        // symlinks on that, then verify it is still inside workDir. This catches
        // symlinks planted inside workDir that point outside (e.g. escape -> /etc).
        var ancestor = destination
        while !FileManager.default.fileExists(atPath: ancestor.path) {
            let parent = ancestor.deletingLastPathComponent()
            if parent.path == ancestor.path { break }
            ancestor = parent
        }
        let resolvedAncestor = ancestor.resolvingSymlinksInPath()

        guard resolvedAncestor.path == resolvedWorkDir.path
            || resolvedAncestor.path.hasPrefix(resolvedWorkDir.path + "/")
        else {
            throw RPCError(
                code: .invalidArgument,
                message: "File path escapes working directory: \(relativePath)"
            )
        }

        return destination
    }

    // MARK: - Manifest building

    /// Walks workDir and returns a FileSyncEntry for every non-temporary regular file.
    /// Exposed for unit testing.
    static func buildManifest(at workDir: URL) throws -> [Wendy_Agent_Services_V1_FileSyncEntry] {
        var entries: [Wendy_Agent_Services_V1_FileSyncEntry] = []

        // Resolve symlinks so paths from the enumerator and from workDir match exactly.
        // On macOS, /tmp and /var are symlinks to /private/tmp and /private/var.
        let resolvedWorkDir = workDir.resolvingSymlinksInPath()
        let workDirPath = resolvedWorkDir.path

        guard FileManager.default.fileExists(atPath: workDirPath) else {
            return entries
        }

        guard
            let enumerator = FileManager.default.enumerator(
                at: resolvedWorkDir,
                includingPropertiesForKeys: [URLResourceKey.isRegularFileKey],
                options: []
            )
        else {
            return entries
        }

        while let fileURL = enumerator.nextObject() as? URL {
            let resourceValues = try fileURL.resourceValues(
                forKeys: Set([URLResourceKey.isRegularFileKey])
            )
            guard resourceValues.isRegularFile == true else { continue }

            // Skip Wendy temporary files.
            if fileURL.lastPathComponent.hasPrefix(".WENDY-") { continue }

            let resolvedFileURL = fileURL.resolvingSymlinksInPath()
            let relativePath = String(resolvedFileURL.path.dropFirst(workDirPath.count + 1))

            // Get POSIX permissions via FileManager.
            let attrs = try FileManager.default.attributesOfItem(atPath: resolvedFileURL.path)
            let mode = (attrs[.posixPermissions] as? Int).map(UInt32.init) ?? 0o644

            // Compute SHA256 by streaming in 64 KiB reads.
            guard let fileHandle = FileHandle(forReadingAtPath: resolvedFileURL.path) else {
                throw RPCError(code: .internalError, message: "Cannot read \(fileURL.path)")
            }
            defer { try? fileHandle.close() }

            var hasher = SHA256()
            var totalSize: Int64 = 0
            while true {
                let chunk = fileHandle.readData(ofLength: 64 * 1024)
                if chunk.isEmpty { break }
                hasher.update(data: chunk)
                totalSize += Int64(chunk.count)
            }

            var entry = Wendy_Agent_Services_V1_FileSyncEntry()
            entry.path = relativePath
            entry.size = totalSize
            entry.sha256 = Data(hasher.finalize())
            entry.mode = mode
            entries.append(entry)
        }

        entries.sort { $0.path < $1.path }
        return entries
    }

    private static func manifestLookup(
        from manifest: [Wendy_Agent_Services_V1_FileSyncEntry],
        in workDir: URL
    ) throws -> [String: Wendy_Agent_Services_V1_FileSyncEntry] {
        var manifestByPath: [String: Wendy_Agent_Services_V1_FileSyncEntry] = [:]
        for entry in manifest {
            _ = try validatedDestination(for: entry.path, in: workDir)
            guard entry.sha256.count == sha256Length else {
                throw RPCError(
                    code: .invalidArgument,
                    message: "Manifest SHA256 must be exactly 32 bytes for \(entry.path)"
                )
            }
            guard manifestByPath[entry.path] == nil else {
                throw RPCError(
                    code: .invalidArgument,
                    message: "Duplicate manifest entry for \(entry.path)"
                )
            }
            manifestByPath[entry.path] = entry
        }
        return manifestByPath
    }

    private static func temporaryURL(for destinationURL: URL, digest: Data) throws -> URL {
        let temporaryName = ".WENDY-\(hexString(digest))~\(destinationURL.lastPathComponent)"
        return destinationURL.deletingLastPathComponent().appendingPathComponent(temporaryName)
    }

    private static func hexString(_ data: Data) -> String {
        data.map { String(format: "%02x", $0) }.joined()
    }
}
