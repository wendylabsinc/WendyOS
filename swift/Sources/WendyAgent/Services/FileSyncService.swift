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
        // Collect all request messages before entering the response producer closure
        // so we can hold a `let` (required by Swift 6 @Sendable capture rules).
        var tempMessages: [Wendy_Agent_Services_V1_FileSyncRequest] = []
        for try await msg in request.messages {
            tempMessages.append(msg)
        }
        let allMessages = tempMessages

        let appsBase = self.appsBase
        let logger = self.logger

        return StreamingServerResponse(metadata: [:]) { writer in
            try await FileSyncService.runSession(
                messages: allMessages,
                writeResponse: { try await writer.write($0) },
                appsBase: appsBase,
                logger: logger
            )
            return Metadata()
        }
    }

    // MARK: - Session logic (nonisolated so it can be captured in @Sendable closures)

    static func runSession(
        messages: [Wendy_Agent_Services_V1_FileSyncRequest],
        writeResponse: (Wendy_Agent_Services_V1_FileSyncResponse) async throws -> Void,
        appsBase: URL,
        logger: Logger
    ) async throws {
        guard let first = messages.first, case .start(let startMsg) = first.requestType else {
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
        var tempHandles: [String: FileHandle] = [:]
        var tempURLs: [String: URL] = [:]

        func cleanupTmp(path: String) {
            if let fh = tempHandles.removeValue(forKey: path) {
                try? fh.close()
            }
            if let url = tempURLs.removeValue(forKey: path) {
                try? FileManager.default.removeItem(at: url)
            }
        }

        do {
            for msg in messages.dropFirst() {
                switch msg.requestType {
                case .chunk(let chunk):
                    let relPath = chunk.path
                    let destURL = workDir.appendingPathComponent(relPath)
                    let tmpURL = URL(fileURLWithPath: destURL.path + ".tmp")

                    if tempHandles[relPath] == nil {
                        try FileManager.default.createDirectory(
                            at: tmpURL.deletingLastPathComponent(),
                            withIntermediateDirectories: true
                        )
                        FileManager.default.createFile(atPath: tmpURL.path, contents: nil)
                        guard let fh = FileHandle(forWritingAtPath: tmpURL.path) else {
                            throw RPCError(
                                code: .internalError,
                                message: "Cannot open temp file at \(tmpURL.path)"
                            )
                        }
                        tempHandles[relPath] = fh
                        tempURLs[relPath] = tmpURL
                    }

                    tempHandles[relPath]!.seekToEndOfFile()
                    tempHandles[relPath]!.write(chunk.data)

                case .commit(let commit):
                    let relPath = commit.path
                    let destURL = workDir.appendingPathComponent(relPath)
                    let tmpURL = tempURLs[relPath] ?? URL(fileURLWithPath: destURL.path + ".tmp")

                    // Close the write handle before reading for verification.
                    if let fh = tempHandles.removeValue(forKey: relPath) {
                        try fh.close()
                    }
                    tempURLs.removeValue(forKey: relPath)

                    // Verify SHA256 and size.
                    guard let tmpData = FileManager.default.contents(atPath: tmpURL.path) else {
                        throw RPCError(
                            code: .internalError,
                            message: "Temp file missing for \(relPath)"
                        )
                    }

                    let actualSize = Int64(tmpData.count)
                    if actualSize != commit.size {
                        try? FileManager.default.removeItem(at: tmpURL)
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "Size mismatch for \(relPath): expected \(commit.size), got \(actualSize)"
                        )
                    }

                    let computedHash = SHA256.hash(data: tmpData)
                        .map { String(format: "%02x", $0) }.joined()
                    if computedHash != commit.sha256 {
                        try? FileManager.default.removeItem(at: tmpURL)
                        throw RPCError(
                            code: .dataLoss,
                            message:
                                "SHA256 mismatch for \(relPath): expected \(commit.sha256), got \(computedHash)"
                        )
                    }

                    // Set file mode from CLI manifest entry; default 0o644.
                    let cliEntry = cliManifest.first(where: { $0.path == relPath })
                    let fileMode = cliEntry.map { Int($0.mode) } ?? 0o644
                    try FileManager.default.setAttributes(
                        [.posixPermissions: fileMode],
                        ofItemAtPath: tmpURL.path
                    )

                    // Atomic rename.
                    try FileManager.default.createDirectory(
                        at: destURL.deletingLastPathComponent(),
                        withIntermediateDirectories: true
                    )
                    if FileManager.default.fileExists(atPath: destURL.path) {
                        try FileManager.default.removeItem(at: destURL)
                    }
                    try FileManager.default.moveItem(at: tmpURL, to: destURL)

                    // Send ack.
                    var ackResponse = Wendy_Agent_Services_V1_FileSyncResponse()
                    var ack = Wendy_Agent_Services_V1_FileSyncAck()
                    ack.path = relPath
                    ackResponse.responseType = .ack(ack)
                    try await writeResponse(ackResponse)

                    logger.info(
                        "File committed",
                        metadata: ["path": "\(relPath)", "app_id": "\(appID)"]
                    )

                case .start, nil:
                    throw RPCError(code: .invalidArgument, message: "Unexpected message in stream")
                }
            }
        } catch {
            // Clean up all open temp handles/files before re-throwing.
            for path in Array(tempHandles.keys) { cleanupTmp(path: path) }
            throw error
        }

        // Clean up any remaining orphaned temp files (shouldn't happen in normal flow).
        for path in Array(tempHandles.keys) { cleanupTmp(path: path) }

        // Prune stale files: in agent manifest but absent from CLI's declared set.
        let cliPaths = Set(cliManifest.map(\.path))
        for entry in agentManifest where !cliPaths.contains(entry.path) {
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

    // MARK: - Manifest building

    /// Walks workDir and returns a FileSyncEntry for every non-.tmp regular file.
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
                options: [.skipsHiddenFiles]
            )
        else {
            return entries
        }

        while let fileURL = enumerator.nextObject() as? URL {
            let resourceValues = try fileURL.resourceValues(
                forKeys: Set([URLResourceKey.isRegularFileKey])
            )
            guard resourceValues.isRegularFile == true else { continue }

            // Skip .tmp files.
            if fileURL.pathExtension == "tmp" { continue }

            let resolvedFileURL = fileURL.resolvingSymlinksInPath()
            let relPath = String(resolvedFileURL.path.dropFirst(workDirPath.count + 1))

            // Get POSIX permissions via FileManager.
            let attrs = try FileManager.default.attributesOfItem(atPath: resolvedFileURL.path)
            let mode = (attrs[.posixPermissions] as? Int).map(UInt32.init) ?? 0o644

            // Compute SHA256 by streaming in 64 KiB reads.
            guard let fh = FileHandle(forReadingAtPath: resolvedFileURL.path) else {
                throw RPCError(code: .internalError, message: "Cannot read \(fileURL.path)")
            }
            defer { try? fh.close() }

            var hasher = SHA256()
            var totalSize: Int64 = 0
            while true {
                let chunk = fh.readData(ofLength: 64 * 1024)
                if chunk.isEmpty { break }
                hasher.update(data: chunk)
                totalSize += Int64(chunk.count)
            }
            let digest = hasher.finalize().map { String(format: "%02x", $0) }.joined()

            var entry = Wendy_Agent_Services_V1_FileSyncEntry()
            entry.path = relPath
            entry.size = totalSize
            entry.sha256 = digest
            entry.mode = mode
            entries.append(entry)
        }

        return entries
    }
}
