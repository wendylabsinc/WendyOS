import CryptoKit
import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import wendy_agent

// MARK: - buildManifest tests

@Suite("FileSyncService.buildManifest")
struct BuildManifestTests {
    @Test("empty directory returns empty manifest")
    func emptyDirectory() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let entries = try FileSyncService.buildManifest(at: URL(fileURLWithPath: temporaryDirectory))
        #expect(entries.isEmpty)
    }

    @Test("single file produces correct path, size, sha256, mode")
    func singleFile() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }

        let content = Data("hello world".utf8)
        let fileURL = URL(fileURLWithPath: temporaryDirectory).appendingPathComponent("app.bin")
        try content.write(to: fileURL)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fileURL.path)

        let entries = try FileSyncService.buildManifest(at: URL(fileURLWithPath: temporaryDirectory))
        #expect(entries.count == 1)
        let entry = entries[0]
        #expect(entry.path == "app.bin")
        #expect(entry.size == Int64(content.count))
        #expect(entry.sha256 == sha256Hex(content))
        #expect(entry.mode == 0o755)
    }

    @Test("nested directories produce correct relative paths")
    func nestedPaths() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }

        let base = URL(fileURLWithPath: temporaryDirectory)
        try FileManager.default.createDirectory(
            at: base.appendingPathComponent("models/v1"),
            withIntermediateDirectories: true
        )
        try Data("weight".utf8).write(to: base.appendingPathComponent("models/v1/weights.bin"))
        try Data("cfg".utf8).write(to: base.appendingPathComponent("config.json"))

        let entries = try FileSyncService.buildManifest(at: base)
        let paths = Set(entries.map(\.path))
        #expect(paths.contains("models/v1/weights.bin"))
        #expect(paths.contains("config.json"))
        #expect(entries.count == 2)
    }

    @Test("Wendy temporary files are excluded from manifest")
    func wendyTemporaryFilesExcluded() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }

        let base = URL(fileURLWithPath: temporaryDirectory)
        try Data("data".utf8).write(to: base.appendingPathComponent("real.bin"))
        try Data("partial".utf8).write(to: base.appendingPathComponent(".WENDY-abc123~real.bin"))

        let entries = try FileSyncService.buildManifest(at: base)
        #expect(entries.count == 1)
        #expect(entries[0].path == "real.bin")
    }

    @Test("non-existent directory returns empty manifest")
    func nonExistentDirectory() throws {
        let missing = URL(
            fileURLWithPath: "/tmp/wendy-test-does-not-exist-\(UUID().uuidString)"
        )
        let entries = try FileSyncService.buildManifest(at: missing)
        #expect(entries.isEmpty)
    }

    @Test("sha256 is correct for known content")
    func sha256Correctness() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }

        let content = Data(repeating: 0xAB, count: 256)
        let fileURL = URL(fileURLWithPath: temporaryDirectory).appendingPathComponent("known.bin")
        try content.write(to: fileURL)

        let entries = try FileSyncService.buildManifest(at: URL(fileURLWithPath: temporaryDirectory))
        #expect(entries.count == 1)
        #expect(entries[0].sha256 == sha256Hex(content))
    }
}

// MARK: - runSession tests

/// These tests drive `FileSyncService.runSession` directly using a mock writer,
/// bypassing the gRPC transport layer entirely.
@Suite("FileSyncService.runSession")
struct RunSessionTests {
    @Test("FileSyncStart against empty dir returns empty FileSyncManifest")
    func startEmptyDirReturnsEmptyManifest() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)

        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = []
        var startReq = Wendy_Agent_Services_V1_FileSyncRequest()
        startReq.requestType = .start(startMsg)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        try await FileSyncService.runSession(
            messages: makeStream([startReq]),
            writeResponse: { responses.append($0) },
            appsBase: appsBaseURL,
            logger: .init(label: "test")
        )

        #expect(responses.count == 2)
        if case .manifest(let m) = responses[0].responseType {
            #expect(m.files.isEmpty)
        } else {
            Issue.record("Expected manifest as first response")
        }
        if case .complete = responses[1].responseType {
            // expected
        } else {
            Issue.record("Expected complete as second response")
        }
    }

    @Test("pre-seeded directory returns correct entries in manifest")
    func startPreSeededDirReturnsManifest() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)

        // Pre-seed the working directory.
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)
        let fileContent = Data("preexisting binary".utf8)
        try fileContent.write(to: appDir.appendingPathComponent("MyApp"))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: appDir.appendingPathComponent("MyApp").path
        )

        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = []
        var startReq = Wendy_Agent_Services_V1_FileSyncRequest()
        startReq.requestType = .start(startMsg)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        try await FileSyncService.runSession(
            messages: makeStream([startReq]),
            writeResponse: { responses.append($0) },
            appsBase: appsBaseURL,
            logger: .init(label: "test")
        )

        if case .manifest(let m) = responses[0].responseType {
            #expect(m.files.count == 1)
            #expect(m.files[0].path == "MyApp")
            #expect(m.files[0].sha256 == sha256Hex(fileContent))
            #expect(m.files[0].size == Int64(fileContent.count))
        } else {
            Issue.record("Expected manifest as first response")
        }
    }

    @Test("upload a file via chunk+commit, file appears at correct path with correct content")
    func uploadFile() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let content = Data("binary content".utf8)
        let sha256 = sha256Hex(content)

        var manifestEntry = Wendy_Agent_Services_V1_FileSyncEntry()
        manifestEntry.path = "MyApp"
        manifestEntry.size = Int64(content.count)
        manifestEntry.sha256 = sha256
        manifestEntry.mode = 0o755

        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = [manifestEntry]

        var chunk = Wendy_Agent_Services_V1_FileSyncChunk()
        chunk.path = "MyApp"
        chunk.data = content

        var commit = Wendy_Agent_Services_V1_FileSyncCommit()
        commit.path = "MyApp"
        commit.sha256 = sha256
        commit.size = Int64(content.count)

        var req0 = Wendy_Agent_Services_V1_FileSyncRequest()
        req0.requestType = .start(startMsg)
        var req1 = Wendy_Agent_Services_V1_FileSyncRequest()
        req1.requestType = .chunk(chunk)
        var req2 = Wendy_Agent_Services_V1_FileSyncRequest()
        req2.requestType = .commit(commit)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        try await FileSyncService.runSession(
            messages: makeStream([req0, req1, req2]),
            writeResponse: { responses.append($0) },
            appsBase: appsBaseURL,
            logger: .init(label: "test")
        )

        // Responses: manifest, ack, complete.
        #expect(responses.count == 3)
        if case .ack(let ack) = responses[1].responseType {
            #expect(ack.path == "MyApp")
        } else {
            Issue.record("Expected ack as second response")
        }
        if case .complete = responses[2].responseType {
            // expected
        } else {
            Issue.record("Expected complete as third response")
        }

        // File exists with correct content and mode.
        let destPath = appsBaseURL.appendingPathComponent(appID).appendingPathComponent("MyApp")
        let written = try Data(contentsOf: destPath)
        #expect(written == content)
        let attrs = try FileManager.default.attributesOfItem(atPath: destPath.path)
        let mode = attrs[.posixPermissions] as? Int
        #expect(mode == 0o755)

        // No .tmp left.
        let temporaryFileURL = URL(fileURLWithPath: destPath.path + ".tmp")
        #expect(!FileManager.default.fileExists(atPath: temporaryFileURL.path))
    }

    @Test("nested path — parent directories created automatically")
    func nestedPath() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let content = Data("config data".utf8)

        var manifestEntry = Wendy_Agent_Services_V1_FileSyncEntry()
        manifestEntry.path = "config/app.json"
        manifestEntry.size = Int64(content.count)
        manifestEntry.sha256 = sha256Hex(content)
        manifestEntry.mode = 0o644

        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = [manifestEntry]

        var chunk = Wendy_Agent_Services_V1_FileSyncChunk()
        chunk.path = "config/app.json"
        chunk.data = content

        var commit = Wendy_Agent_Services_V1_FileSyncCommit()
        commit.path = "config/app.json"
        commit.sha256 = sha256Hex(content)
        commit.size = Int64(content.count)

        var req0 = Wendy_Agent_Services_V1_FileSyncRequest()
        req0.requestType = .start(startMsg)
        var req1 = Wendy_Agent_Services_V1_FileSyncRequest()
        req1.requestType = .chunk(chunk)
        var req2 = Wendy_Agent_Services_V1_FileSyncRequest()
        req2.requestType = .commit(commit)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        try await FileSyncService.runSession(
            messages: makeStream([req0, req1, req2]),
            writeResponse: { responses.append($0) },
            appsBase: appsBaseURL,
            logger: .init(label: "test")
        )

        let destPath =
            appsBaseURL
            .appendingPathComponent(appID)
            .appendingPathComponent("config/app.json")
        let written = try Data(contentsOf: destPath)
        #expect(written == content)
    }

    @Test("corrupt commit — wrong SHA256 — returns error, no file or .tmp")
    func corruptCommitNoPartialFile() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let content = Data("real data".utf8)

        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = []

        var chunk = Wendy_Agent_Services_V1_FileSyncChunk()
        chunk.path = "app"
        chunk.data = content

        var commit = Wendy_Agent_Services_V1_FileSyncCommit()
        commit.path = "app"
        commit.sha256 = String(repeating: "a", count: 64)  // wrong hash
        commit.size = Int64(content.count)

        var req0 = Wendy_Agent_Services_V1_FileSyncRequest()
        req0.requestType = .start(startMsg)
        var req1 = Wendy_Agent_Services_V1_FileSyncRequest()
        req1.requestType = .chunk(chunk)
        var req2 = Wendy_Agent_Services_V1_FileSyncRequest()
        req2.requestType = .commit(commit)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        do {
            try await FileSyncService.runSession(
                messages: makeStream([req0, req1, req2]),
                writeResponse: { responses.append($0) },
                appsBase: appsBaseURL,
                logger: .init(label: "test")
            )
            Issue.record("Expected error from corrupt commit")
        } catch {
            // Expected: SHA256 mismatch.
        }

        // No file or .tmp should remain.
        let appDir = appsBaseURL.appendingPathComponent(appID)
        let destPath = appDir.appendingPathComponent("app")
        let temporaryFileURL = URL(fileURLWithPath: destPath.path + ".tmp")
        #expect(!FileManager.default.fileExists(atPath: destPath.path))
        #expect(!FileManager.default.fileExists(atPath: temporaryFileURL.path))
    }

    @Test("stale file is deleted after stream EOF")
    func staleFileDeleted() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)

        // Pre-seed with a file the CLI manifest won't declare.
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)
        let stalePath = appDir.appendingPathComponent("old.bin")
        try Data("stale".utf8).write(to: stalePath)

        // Sync with empty CLI manifest.
        var startMsg = Wendy_Agent_Services_V1_FileSyncStart()
        startMsg.appID = appID
        startMsg.manifest = []
        var req0 = Wendy_Agent_Services_V1_FileSyncRequest()
        req0.requestType = .start(startMsg)

        var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []
        try await FileSyncService.runSession(
            messages: makeStream([req0]),
            writeResponse: { responses.append($0) },
            appsBase: appsBaseURL,
            logger: .init(label: "test")
        )

        #expect(!FileManager.default.fileExists(atPath: stalePath.path))
        // FileSyncComplete should be the last response.
        if case .complete = responses.last?.responseType {
            // expected
        } else {
            Issue.record("Expected FileSyncComplete as last response")
        }
    }
}

// MARK: - Helpers

private func makeStream(
    _ messages: [Wendy_Agent_Services_V1_FileSyncRequest]
) -> AsyncStream<Wendy_Agent_Services_V1_FileSyncRequest> {
    AsyncStream { continuation in
        for message in messages { continuation.yield(message) }
        continuation.finish()
    }
}

private func makeTempDir() throws -> String {
    let path =
        FileManager.default.temporaryDirectory
        .appendingPathComponent("wendy-test-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
    return path
}

private func cleanup(_ path: String) {
    try? FileManager.default.removeItem(atPath: path)
}

private func sha256Hex(_ data: Data) -> String {
    let hash = SHA256.hash(data: data)
    return hash.map { String(format: "%02x", $0) }.joined()
}
