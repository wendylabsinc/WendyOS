import CryptoKit
import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

// MARK: - buildManifest tests

@Suite("FileSyncService.buildManifest")
struct BuildManifestTests {
    @Test("single file produces correct path, size, sha256 bytes, and mode")
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
        #expect(entry.sha256 == sha256Digest(content))
        #expect(entry.sha256.count == 32)
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

    @Test("non-existent directory returns empty manifest")
    func nonExistentDirectory() throws {
        let missing = URL(
            fileURLWithPath: "/tmp/wendy-test-does-not-exist-\(UUID().uuidString)"
        )
        let entries = try FileSyncService.buildManifest(at: missing)
        #expect(entries.isEmpty)
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
}

// MARK: - runSession tests

@Suite("FileSyncService.runSession")
struct RunSessionTests {
    @Test("FileSyncStart against empty dir returns empty manifest and complete")
    func startEmptyDirReturnsEmptyManifest() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let responses = try await runSession(
            messages: [startRequest(appID: "sh.wendy.TestApp", manifest: [])],
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(responses.count == 2)
        if case .manifest(let manifest) = responses[0].responseType {
            #expect(manifest.files.isEmpty)
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

        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)
        let fileContent = Data("preexisting binary".utf8)
        try fileContent.write(to: appDir.appendingPathComponent("MyApp"))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: appDir.appendingPathComponent("MyApp").path
        )

        let responses = try await runSession(
            messages: [startRequest(appID: appID, manifest: [])],
            appsBase: appsBaseURL
        )

        if case .manifest(let manifest) = responses[0].responseType {
            #expect(manifest.files.count == 1)
            #expect(manifest.files[0].path == "MyApp")
            #expect(manifest.files[0].sha256 == sha256Digest(fileContent))
            #expect(manifest.files[0].size == Int64(fileContent.count))
        } else {
            Issue.record("Expected manifest as first response")
        }
    }

    @Test("duplicate manifest path rejected")
    func duplicateManifestPathRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let digest = sha256Digest(Data("same".utf8))
        let manifest = [
            manifestEntry(path: "app", content: Data("same".utf8), mode: 0o644),
            entry(path: "app", size: 4, sha256: digest, mode: 0o755),
        ]

        await expectRunSessionFailure(
            messages: [startRequest(appID: "sh.wendy.TestApp", manifest: manifest)],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("invalid 32-byte digest length rejected")
    func invalidDigestLengthRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let manifest = [entry(path: "app", size: 1, sha256: Data([0x00]), mode: 0o644)]
        await expectRunSessionFailure(
            messages: [startRequest(appID: "sh.wendy.TestApp", manifest: manifest)],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("multi-chunk happy path")
    func multiChunkHappyPath() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)

        let chunk1 = Data("hello ".utf8)
        let chunk2 = Data("world".utf8)
        let fullContent = chunk1 + chunk2
        let finalDigest = sha256Digest(fullContent)
        let manifest = [entry(path: "MyApp", size: Int64(fullContent.count), sha256: finalDigest, mode: 0o755)]

        let responses = try await runSession(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(path: "MyApp", data: chunk1, sequence: 0, cumulativeSize: Int64(chunk1.count), sha256: sha256Digest(chunk1)),
                chunkRequest(path: "MyApp", data: chunk2, sequence: 1, cumulativeSize: Int64(fullContent.count), sha256: finalDigest),
                commitRequest(path: "MyApp", size: Int64(fullContent.count), sha256: finalDigest),
            ],
            appsBase: appsBaseURL
        )

        #expect(responses.count == 3)
        if case .ack(let ack) = responses[1].responseType {
            #expect(ack.path == "MyApp")
        } else {
            Issue.record("Expected ack as second response")
        }

        let destinationURL = appsBaseURL.appendingPathComponent(appID).appendingPathComponent("MyApp")
        #expect(try Data(contentsOf: destinationURL) == fullContent)
        #expect(try permissions(of: destinationURL) == 0o755)
    }

    @Test("nested path creates parent directories automatically")
    func nestedPathCreatesParentDirectories() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let content = Data("config data".utf8)
        let digest = sha256Digest(content)
        let manifest = [entry(path: "config/app.json", size: Int64(content.count), sha256: digest, mode: 0o644)]

        _ = try await runSession(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(path: "config/app.json", data: content, sequence: 0, cumulativeSize: Int64(content.count), sha256: digest),
                commitRequest(path: "config/app.json", size: Int64(content.count), sha256: digest),
            ],
            appsBase: appsBaseURL
        )

        let destinationURL = appsBaseURL
            .appendingPathComponent(appID)
            .appendingPathComponent("config/app.json")
        #expect(try Data(contentsOf: destinationURL) == content)
    }

    @Test("wrong cumulative hash fails immediately and removes temp file")
    func wrongCumulativeHashFailsImmediately() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let content = Data("hello".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]
        let tempURL = temporaryURL(
            appsBase: URL(fileURLWithPath: appsBase),
            appID: appID,
            path: "app",
            digest: manifest[0].sha256
        )
        let destinationURL = URL(fileURLWithPath: appsBase)
            .appendingPathComponent(appID)
            .appendingPathComponent("app")

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(
                    path: "app",
                    data: content,
                    sequence: 0,
                    cumulativeSize: Int64(content.count),
                    sha256: Data(repeating: 0xAA, count: 32)
                ),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(!FileManager.default.fileExists(atPath: tempURL.path))
        #expect(!FileManager.default.fileExists(atPath: destinationURL.path))
    }

    @Test("wrong cumulative size fails")
    func wrongCumulativeSizeFails() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("hello".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(
                    path: "app",
                    data: content,
                    sequence: 0,
                    cumulativeSize: Int64(content.count - 1),
                    sha256: sha256Digest(content)
                ),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("wrong sequence fails")
    func wrongSequenceFails() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("hello world".utf8)
        let first = Data("hello ".utf8)
        let second = Data("world".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "app", data: first, sequence: 0, cumulativeSize: Int64(first.count), sha256: sha256Digest(first)),
                chunkRequest(path: "app", data: second, sequence: 2, cumulativeSize: Int64(content.count), sha256: sha256Digest(content)),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("cumulative size exceeding manifest size fails early")
    func cumulativeSizeExceedingManifestSizeFailsEarly() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let manifest = [entry(path: "app", size: 3, sha256: sha256Digest(Data("hey".utf8)), mode: 0o644)]
        let content = Data("hello".utf8)

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "app", data: content, sequence: 0, cumulativeSize: Int64(content.count), sha256: sha256Digest(content)),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("zero-length chunk for non-empty file rejected")
    func zeroLengthChunkForNonEmptyFileRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("hello".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "app", data: Data(), sequence: 0, cumulativeSize: 0, sha256: sha256Digest(Data())),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("undeclared path rejected")
    func undeclaredPathRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let manifest = [manifestEntry(path: "app", content: Data("hello".utf8), mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "other", data: Data("hello".utf8), sequence: 0, cumulativeSize: 5, sha256: sha256Digest(Data("hello".utf8))),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("switching paths mid-file rejected")
    func switchingPathsMidFileRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let contentA = Data("hello".utf8)
        let contentB = Data("world".utf8)
        let manifest = [
            manifestEntry(path: "a", content: contentA, mode: 0o644),
            manifestEntry(path: "b", content: contentB, mode: 0o644),
        ]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "a", data: contentA.prefixData(2), sequence: 0, cumulativeSize: 2, sha256: sha256Digest(contentA.prefixData(2))),
                chunkRequest(path: "b", data: contentB, sequence: 0, cumulativeSize: Int64(contentB.count), sha256: sha256Digest(contentB)),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("commit must match manifest and removes temp file on failure")
    func commitMustMatchManifest() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let content = Data("hello".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]
        let tempURL = temporaryURL(
            appsBase: URL(fileURLWithPath: appsBase),
            appID: appID,
            path: "app",
            digest: manifest[0].sha256
        )

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(path: "app", data: content, sequence: 0, cumulativeSize: 5, sha256: sha256Digest(content)),
                commitRequest(path: "app", size: 5, sha256: Data(repeating: 0xBB, count: 32)),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(!FileManager.default.fileExists(atPath: tempURL.path))
    }

    @Test("commit must match in-memory final state")
    func commitMustMatchInMemoryFinalState() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let manifestContent = Data("hello".utf8)
        let transferredContent = Data("HELLO".utf8)
        let manifest = [manifestEntry(path: "app", content: manifestContent, mode: 0o644)]
        let destinationURL = URL(fileURLWithPath: appsBase)
            .appendingPathComponent(appID)
            .appendingPathComponent("app")

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(path: "app", data: transferredContent, sequence: 0, cumulativeSize: 5, sha256: sha256Digest(transferredContent)),
                commitRequest(path: "app", size: 5, sha256: manifest[0].sha256),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(!FileManager.default.fileExists(atPath: destinationURL.path))
    }

    @Test("duplicate commit rejected")
    func duplicateCommitRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("hello".utf8)
        let digest = sha256Digest(content)
        let manifest = [entry(path: "app", size: 5, sha256: digest, mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "app", data: content, sequence: 0, cumulativeSize: 5, sha256: digest),
                commitRequest(path: "app", size: 5, sha256: digest),
                commitRequest(path: "app", size: 5, sha256: digest),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("no reread required at commit time")
    func noRereadRequired() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let content = Data("hello world".utf8)
        let digest = sha256Digest(content)
        let manifest = [entry(path: "app", size: Int64(content.count), sha256: digest, mode: 0o644)]
        let tempURL = temporaryURL(appsBase: appsBaseURL, appID: appID, path: "app", digest: digest)
        let destinationURL = appsBaseURL.appendingPathComponent(appID).appendingPathComponent("app")

        let recorder = ResponseRecorder()
        var continuation: AsyncStream<Wendy_Agent_Services_V1_FileSyncRequest>.Continuation!
        let stream = AsyncStream<Wendy_Agent_Services_V1_FileSyncRequest> { continuation = $0 }

        let task = Task {
            try await FileSyncService.runSession(
                messages: stream,
                writeResponse: { response in await recorder.append(response) },
                appsBase: appsBaseURL,
                logger: .init(label: "test")
            )
        }

        continuation.yield(startRequest(appID: appID, manifest: manifest))
        continuation.yield(chunkRequest(path: "app", data: content, sequence: 0, cumulativeSize: Int64(content.count), sha256: digest))
        try await waitForFile(at: tempURL)
        try FileManager.default.setAttributes([.posixPermissions: 0o000], ofItemAtPath: tempURL.path)
        continuation.yield(commitRequest(path: "app", size: Int64(content.count), sha256: digest))
        continuation.finish()

        try await task.value

        let responses = await recorder.snapshot()
        #expect(responses.count == 3)
        #expect(try Data(contentsOf: destinationURL) == content)
    }

    @Test("empty file via one empty chunk and commit succeeds")
    func emptyFileViaOneChunkAndCommitSucceeds() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let digest = sha256Digest(Data())
        let manifest = [entry(path: "Models/.gitkeep", size: 0, sha256: digest, mode: 0o644)]

        let responses = try await runSession(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chunkRequest(path: "Models/.gitkeep", data: Data(), sequence: 0, cumulativeSize: 0, sha256: digest),
                commitRequest(path: "Models/.gitkeep", size: 0, sha256: digest),
            ],
            appsBase: appsBaseURL
        )

        #expect(responses.count == 3)
        let destinationURL = appsBaseURL
            .appendingPathComponent(appID)
            .appendingPathComponent("Models/.gitkeep")
        #expect(try Data(contentsOf: destinationURL).isEmpty)
    }

    @Test("mode-only update succeeds for existing unchanged file")
    func modeOnlyUpdateSucceeds() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)

        let content = Data("config".utf8)
        let fileURL = appDir.appendingPathComponent("config.json")
        try content.write(to: fileURL)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: fileURL.path)

        let manifest = [entry(path: "config.json", size: Int64(content.count), sha256: sha256Digest(content), mode: 0o755)]
        let responses = try await runSession(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chmodRequest(path: "config.json", mode: 0o755, size: Int64(content.count), sha256: sha256Digest(content)),
            ],
            appsBase: appsBaseURL
        )

        #expect(responses.count == 3)
        if case .ack(let ack) = responses[1].responseType {
            #expect(ack.path == "config.json")
        } else {
            Issue.record("Expected ack as second response")
        }
        #expect(try permissions(of: fileURL) == 0o755)
        #expect(try Data(contentsOf: fileURL) == content)
    }

    @Test("missing target file fails for mode-only update")
    func modeOnlyMissingTargetFails() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("config".utf8)
        let digest = sha256Digest(content)
        let manifest = [entry(path: "config.json", size: Int64(content.count), sha256: digest, mode: 0o755)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chmodRequest(path: "config.json", mode: 0o755, size: Int64(content.count), sha256: digest),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("mode-only during active transfer rejected")
    func modeOnlyDuringActiveTransferRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let contentA = Data("hello".utf8)
        let contentB = Data("world".utf8)
        let manifest = [
            manifestEntry(path: "a", content: contentA, mode: 0o644),
            manifestEntry(path: "b", content: contentB, mode: 0o755),
        ]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "a", data: contentA, sequence: 0, cumulativeSize: 5, sha256: sha256Digest(contentA)),
                chmodRequest(path: "b", mode: 0o755, size: 5, sha256: sha256Digest(contentB)),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }

    @Test("duplicate finalized-path mode update rejected")
    func duplicateFinalizedPathModeUpdateRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)

        let content = Data("config".utf8)
        let fileURL = appDir.appendingPathComponent("config.json")
        try content.write(to: fileURL)

        let digest = sha256Digest(content)
        let manifest = [entry(path: "config.json", size: Int64(content.count), sha256: digest, mode: 0o755)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: appID, manifest: manifest),
                chmodRequest(path: "config.json", mode: 0o755, size: Int64(content.count), sha256: digest),
                chmodRequest(path: "config.json", mode: 0o755, size: Int64(content.count), sha256: digest),
            ],
            appsBase: appsBaseURL
        )
    }

    @Test("stale file pruning still works")
    func staleFilePruningStillWorks() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)
        let staleURL = appDir.appendingPathComponent("old.bin")
        try Data("stale".utf8).write(to: staleURL)

        let responses = try await runSession(
            messages: [
                startRequest(appID: appID, manifest: []),
                deleteRequest(paths: ["old.bin"]),
            ],
            appsBase: appsBaseURL
        )

        #expect(responses.count == 2)
        #expect(!FileManager.default.fileExists(atPath: staleURL.path))
    }

    @Test("files absent from manifest are not pruned implicitly")
    func filesAbsentFromManifestAreNotPrunedImplicitly() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let appID = "sh.wendy.TestApp"
        let appsBaseURL = URL(fileURLWithPath: appsBase)
        let appDir = appsBaseURL.appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDir, withIntermediateDirectories: true)
        let staleURL = appDir.appendingPathComponent("old.bin")
        try Data("stale".utf8).write(to: staleURL)

        _ = try await runSession(
            messages: [startRequest(appID: appID, manifest: [])],
            appsBase: appsBaseURL
        )

        #expect(FileManager.default.fileExists(atPath: staleURL.path))
    }

    @Test("missing delete target is ignored")
    func missingDeleteTargetIsIgnored() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let responses = try await runSession(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: []),
                deleteRequest(paths: ["missing.bin"]),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(responses.count == 2)
    }

    @Test("delete during active transfer rejected")
    func deleteDuringActiveTransferRejected() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }
        let content = Data("hello".utf8)
        let manifest = [manifestEntry(path: "app", content: content, mode: 0o644)]

        await expectRunSessionFailure(
            messages: [
                startRequest(appID: "sh.wendy.TestApp", manifest: manifest),
                chunkRequest(path: "app", data: content, sequence: 0, cumulativeSize: 5, sha256: sha256Digest(content)),
                deleteRequest(paths: ["old.bin"]),
            ],
            appsBase: URL(fileURLWithPath: appsBase)
        )
    }
}

// MARK: - validatedDestination tests

@Suite("FileSyncService.validatedDestination")
struct ValidatedDestinationTests {
    @Test("simple relative path is accepted")
    func simpleRelativePath() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        let result = try FileSyncService.validatedDestination(for: "MyApp", in: workDir)
        #expect(result.path == workDir.appendingPathComponent("MyApp").path)
    }

    @Test("nested relative path is accepted")
    func nestedRelativePath() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        let result = try FileSyncService.validatedDestination(for: "config/app.json", in: workDir)
        #expect(result.path == workDir.appendingPathComponent("config/app.json").path)
    }

    @Test("hidden file is accepted")
    func hiddenFile() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        let result = try FileSyncService.validatedDestination(for: ".config", in: workDir)
        #expect(result.path == workDir.appendingPathComponent(".config").path)
    }

    @Test("empty path is rejected")
    func emptyPath() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        #expect(throws: (any Error).self) {
            try FileSyncService.validatedDestination(for: "", in: workDir)
        }
    }

    @Test("absolute path is rejected")
    func absolutePath() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        #expect(throws: (any Error).self) {
            try FileSyncService.validatedDestination(for: "/etc/passwd", in: workDir)
        }
    }

    @Test(".. component is rejected")
    func dotDotComponent() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        #expect(throws: (any Error).self) {
            try FileSyncService.validatedDestination(for: "../../etc/passwd", in: workDir)
        }
    }

    @Test(". component is rejected")
    func dotComponent() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }
        let workDir = URL(fileURLWithPath: temporaryDirectory)
        #expect(throws: (any Error).self) {
            try FileSyncService.validatedDestination(for: "config/./app.json", in: workDir)
        }
    }

    @Test("symlink escaping workDir is rejected")
    func symlinkEscape() throws {
        let temporaryDirectory = try makeTempDir()
        defer { cleanup(temporaryDirectory) }

        let workDir = URL(fileURLWithPath: temporaryDirectory)
        let symlinkURL = workDir.appendingPathComponent("escape")
        try FileManager.default.createSymbolicLink(atPath: symlinkURL.path, withDestinationPath: "/tmp")

        #expect(throws: (any Error).self) {
            try FileSyncService.validatedDestination(for: "escape/passwd", in: workDir)
        }
    }
}

// MARK: - Helpers

private actor ResponseRecorder {
    private var responses: [Wendy_Agent_Services_V1_FileSyncResponse] = []

    func append(_ response: Wendy_Agent_Services_V1_FileSyncResponse) {
        responses.append(response)
    }

    func snapshot() -> [Wendy_Agent_Services_V1_FileSyncResponse] {
        responses
    }
}

private func runSession(
    messages: [Wendy_Agent_Services_V1_FileSyncRequest],
    appsBase: URL
) async throws -> [Wendy_Agent_Services_V1_FileSyncResponse] {
    let recorder = ResponseRecorder()
    try await FileSyncService.runSession(
        messages: makeStream(messages),
        writeResponse: { response in await recorder.append(response) },
        appsBase: appsBase,
        logger: .init(label: "test")
    )
    return await recorder.snapshot()
}

private func expectRunSessionFailure(
    messages: [Wendy_Agent_Services_V1_FileSyncRequest],
    appsBase: URL
) async {
    do {
        _ = try await runSession(messages: messages, appsBase: appsBase)
        Issue.record("Expected FileSyncService.runSession to throw")
    } catch {
        // expected
    }
}

private func startRequest(
    appID: String,
    manifest: [Wendy_Agent_Services_V1_FileSyncEntry]
) -> Wendy_Agent_Services_V1_FileSyncRequest {
    var start = Wendy_Agent_Services_V1_FileSyncStart()
    start.appID = appID
    start.manifest = .with { $0.files = manifest }

    var request = Wendy_Agent_Services_V1_FileSyncRequest()
    request.requestType = .start(start)
    return request
}

private func chunkRequest(
    path: String,
    data: Data,
    sequence: UInt64,
    cumulativeSize: Int64,
    sha256: Data
) -> Wendy_Agent_Services_V1_FileSyncRequest {
    var chunk = Wendy_Agent_Services_V1_FileSyncChunk()
    chunk.path = path
    chunk.data = data
    chunk.sequence = sequence
    chunk.cumulativeSize = cumulativeSize
    chunk.sha256 = sha256

    var request = Wendy_Agent_Services_V1_FileSyncRequest()
    request.requestType = .chunk(chunk)
    return request
}

private func commitRequest(
    path: String,
    size: Int64,
    sha256: Data
) -> Wendy_Agent_Services_V1_FileSyncRequest {
    var commit = Wendy_Agent_Services_V1_FileSyncCommit()
    commit.path = path
    commit.size = size
    commit.sha256 = sha256

    var request = Wendy_Agent_Services_V1_FileSyncRequest()
    request.requestType = .commit(commit)
    return request
}

private func chmodRequest(
    path: String,
    mode: UInt32,
    size: Int64,
    sha256: Data
) -> Wendy_Agent_Services_V1_FileSyncRequest {
    var chmod = Wendy_Agent_Services_V1_FileSyncChmod()
    chmod.path = path
    chmod.mode = mode
    chmod.size = size
    chmod.sha256 = sha256

    var request = Wendy_Agent_Services_V1_FileSyncRequest()
    request.requestType = .chmod(chmod)
    return request
}

private func deleteRequest(paths: [String]) -> Wendy_Agent_Services_V1_FileSyncRequest {
    var deleteRequest = Wendy_Agent_Services_V1_FileSyncDelete()
    deleteRequest.paths = paths

    var request = Wendy_Agent_Services_V1_FileSyncRequest()
    request.requestType = .delete(deleteRequest)
    return request
}

private func manifestEntry(path: String, content: Data, mode: UInt32) -> Wendy_Agent_Services_V1_FileSyncEntry {
    entry(path: path, size: Int64(content.count), sha256: sha256Digest(content), mode: mode)
}

private func entry(
    path: String,
    size: Int64,
    sha256: Data,
    mode: UInt32
) -> Wendy_Agent_Services_V1_FileSyncEntry {
    var entry = Wendy_Agent_Services_V1_FileSyncEntry()
    entry.path = path
    entry.size = size
    entry.sha256 = sha256
    entry.mode = mode
    return entry
}

private func temporaryURL(appsBase: URL, appID: String, path: String, digest: Data) -> URL {
    let destinationURL = appsBase.appendingPathComponent(appID).appendingPathComponent(path)
    let temporaryName = ".WENDY-\(hexString(digest))~\(destinationURL.lastPathComponent)"
    return destinationURL.deletingLastPathComponent().appendingPathComponent(temporaryName)
}

private func waitForFile(at url: URL, timeoutNanoseconds: UInt64 = 1_000_000_000) async throws {
    let deadline = DispatchTime.now().uptimeNanoseconds + timeoutNanoseconds
    while DispatchTime.now().uptimeNanoseconds < deadline {
        if FileManager.default.fileExists(atPath: url.path) {
            return
        }
        try await Task.sleep(nanoseconds: 10_000_000)
    }
    throw NSError(domain: "FileSyncServiceTests", code: 1, userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for \(url.path)"])
}

private func permissions(of url: URL) throws -> Int {
    let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
    return (attributes[.posixPermissions] as? Int) ?? -1
}

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

private func sha256Digest(_ data: Data) -> Data {
    Data(SHA256.hash(data: data))
}

private func hexString(_ data: Data) -> String {
    data.map { String(format: "%02x", $0) }.joined()
}

private extension Data {
    func prefixData(_ count: Int) -> Data {
        Data(prefix(count))
    }
}
