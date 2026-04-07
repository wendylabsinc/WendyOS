import CryptoKit
import Foundation
import GRPCCore
import Logging
import WendyAgentGRPC

/// FileSyncService implements the WendyFileSyncService gRPC protocol.
/// Each app gets an isolated working directory under `appsBase/<appId>`.
actor FileSyncService: Wendy_Agent_Services_V1_WendyFileSyncService.ServiceProtocol {
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
        let cliManifest = startMsg.manifest
        let workDir = appsBase.appendingPathComponent(appID)

        // Ensure working directory exists.
        try FileManager.default.createDirectory(at: workDir, withIntermediateDirectories: true)

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

        // Process remaining messages: chunks and commits.
        var temporaryHandles: [String: FileHandle] = [:]
        var temporaryURLs: [String: URL] = [:]

        func cleanupTemporary(path: String) {
            if let fileHandle = temporaryHandles.removeValue(forKey: path) {
                try? fileHandle.close()
            }
            if let url = temporaryURLs.removeValue(forKey: path) {
                try? FileManager.default.removeItem(at: url)
            }
        }

        do {
            while let message = try await messageIterator.next() {
                switch message.requestType {
                case .chunk(let chunk):
                    let relativePath = chunk.path
                    let destinationURL = try FileSyncService.validatedDestination(
                        for: relativePath, in: workDir
                    )

                    if temporaryHandles[relativePath] == nil {
                        guard let entry = cliManifest.first(where: { $0.path == relativePath }) else {
                            throw RPCError(
                                code: .invalidArgument,
                                message: "No manifest entry for \(relativePath)"
                            )
                        }
                        let temporaryName = ".WENDY-\(entry.sha256)~\(destinationURL.lastPathComponent)"
                        let temporaryURL = destinationURL.deletingLastPathComponent().appendingPathComponent(temporaryName)
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
                        temporaryHandles[relativePath] = fileHandle
                        temporaryURLs[relativePath] = temporaryURL
                    }

                    temporaryHandles[relativePath]!.seekToEndOfFile()
                    try temporaryHandles[relativePath]!.write(contentsOf: chunk.data)

                case .commit(let commit):
                    let relativePath = commit.path
                    let destinationURL = try FileSyncService.validatedDestination(
                        for: relativePath, in: workDir
                    )

                    guard let temporaryURL = temporaryURLs.removeValue(forKey: relativePath) else {
                        throw RPCError(
                            code: .invalidArgument,
                            message: "No chunks received for \(relativePath)"
                        )
                    }

                    // Close the write handle before reading for verification.
                    if let fileHandle = temporaryHandles.removeValue(forKey: relativePath) {
                        try fileHandle.close()
                    }

                    // Verify SHA256 and size by streaming in 64 KiB reads.
                    guard let readHandle = FileHandle(forReadingAtPath: temporaryURL.path) else {
                        throw RPCError(
                            code: .internalError,
                            message: "Temporary file missing for \(relativePath)"
                        )
                    }
                    defer { try? readHandle.close() }

                    var hasher = SHA256()
                    var actualSize: Int64 = 0
                    while true {
                        let chunk = readHandle.readData(ofLength: 64 * 1024)
                        if chunk.isEmpty { break }
                        hasher.update(data: chunk)
                        actualSize += Int64(chunk.count)
                    }

                    if actualSize != commit.size {
                        try? FileManager.default.removeItem(at: temporaryURL)
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "Size mismatch for \(relativePath): expected \(commit.size), got \(actualSize)"
                        )
                    }

                    let computedHash = hasher.finalize()
                        .map { String(format: "%02x", $0) }.joined()
                    if computedHash != commit.sha256 {
                        try? FileManager.default.removeItem(at: temporaryURL)
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "SHA256 mismatch for \(relativePath): expected \(commit.sha256), got \(computedHash)"
                        )
                    }

                    // Set file mode from CLI manifest entry; default 0o644.
                    let cliEntry = cliManifest.first(where: { $0.path == relativePath })
                    let fileMode = cliEntry.map { Int($0.mode) } ?? 0o644
                    try FileManager.default.setAttributes(
                        [.posixPermissions: fileMode],
                        ofItemAtPath: temporaryURL.path
                    )

                    // Atomic rename.
                    try FileManager.default.createDirectory(
                        at: destinationURL.deletingLastPathComponent(),
                        withIntermediateDirectories: true
                    )
                    if FileManager.default.fileExists(atPath: destinationURL.path) {
                        try FileManager.default.removeItem(at: destinationURL)
                    }
                    try FileManager.default.moveItem(at: temporaryURL, to: destinationURL)

                    // Send ack.
                    var ackResponse = Wendy_Agent_Services_V1_FileSyncResponse()
                    var ack = Wendy_Agent_Services_V1_FileSyncAck()
                    ack.path = relativePath
                    ackResponse.responseType = .ack(ack)
                    try await writeResponse(ackResponse)

                    logger.info(
                        "File committed",
                        metadata: ["path": "\(relativePath)", "app_id": "\(appID)"]
                    )

                case .start, nil:
                    throw RPCError(code: .invalidArgument, message: "Unexpected message in stream")
                }
            }
        } catch {
            // Clean up all open temporary handles/files before re-throwing.
            for path in Array(temporaryHandles.keys) { cleanupTemporary(path: path) }
            throw error
        }

        // Clean up any remaining orphaned temporary files (shouldn't happen in normal flow).
        for path in Array(temporaryHandles.keys) { cleanupTemporary(path: path) }

        // Prune stale files: on disk after the session but absent from CLI's declared set.
        let postSessionManifest = try buildManifest(at: workDir)
        let cliPaths = Set(cliManifest.map(\.path))
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
            let digest = hasher.finalize().map { String(format: "%02x", $0) }.joined()

            var entry = Wendy_Agent_Services_V1_FileSyncEntry()
            entry.path = relativePath
            entry.size = totalSize
            entry.sha256 = digest
            entry.mode = mode
            entries.append(entry)
        }

        return entries
    }
}
