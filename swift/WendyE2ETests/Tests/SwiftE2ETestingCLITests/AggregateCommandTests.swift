import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct `aggregate command` {
    @Test
    func `stores attempt artifacts once and observations separately`() throws {
        let rootURL = aggregateTemporaryDirectory()
        defer { try? FileManager.default.removeItem(at: rootURL) }

        let outputURL = rootURL.appendingPathComponent("aggregate", isDirectory: true)
        let packageURL = rootURL.appendingPathComponent("package", isDirectory: true)
        try writeAggregateTestSource(in: packageURL)
        let attemptURL = rootURL.appendingPathComponent(
            "swift-e2e-tests.local0000.macos-to-rpi.0001",
            isDirectory: true
        )
        let observationURL =
            attemptURL
            .appendingPathComponent("observations", isDirectory: true)
            .appendingPathComponent("wendy-device-info", isDirectory: true)
            .appendingPathComponent("prints-json-device-information", isDirectory: true)

        try FileManager.default.createDirectory(
            at: observationURL,
            withIntermediateDirectories: true
        )
        try "{}\n".write(
            to: attemptURL.appendingPathComponent("attempt.json"),
            atomically: true,
            encoding: .utf8
        )
        try "<testsuite />\n".write(
            to: attemptURL.appendingPathComponent("test-results.xml"),
            atomically: true,
            encoding: .utf8
        )
        try "preflight log\n".write(
            to: attemptURL.appendingPathComponent("attempt.log"),
            atomically: true,
            encoding: .utf8
        )
        try "# Recording\n".write(
            to: observationURL.appendingPathComponent("recording.md"),
            atomically: true,
            encoding: .utf8
        )
        try writeAggregateTestMetadata(to: observationURL)

        for relative in [
            "coverage/bin/wendy", "coverage/bootstrap/covmeta.startup",
            "coverage/go-native.tar.gz", "coverage/coverage.out", "report/index.html",
        ] {
            let url = attemptURL.appendingPathComponent(relative)
            try FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            try "payload kept only in raw evidence".write(
                to: url,
                atomically: true,
                encoding: .utf8
            )
        }
        for name in ["source.json", "summary.json", "test-0.xml"] {
            try "provenance and failure evidence".write(
                to: attemptURL.appendingPathComponent("coverage/\(name)"),
                atomically: true,
                encoding: .utf8
            )
        }
        try FileManager.default.createDirectory(
            at: observationURL.appendingPathComponent("coverage/go"),
            withIntermediateDirectories: true
        )
        try "native".write(
            to: observationURL.appendingPathComponent("coverage/go/covmeta.test"),
            atomically: true,
            encoding: .utf8
        )

        var command = try AggregateCommand.parse([
            "--output-dir", outputURL.path,
            "--package-dir", packageURL.path,
            attemptURL.path,
        ])
        try command.run()

        let runURL = outputURL.appendingPathComponent(
            "swift-e2e-tests.local0000",
            isDirectory: true
        )
        let attemptArtifactsURL =
            runURL
            .appendingPathComponent("attempts", isDirectory: true)
            .appendingPathComponent("macos-to-rpi", isDirectory: true)
            .appendingPathComponent("0001", isDirectory: true)
        let aggregateObservationURL =
            runURL
            .appendingPathComponent("observations", isDirectory: true)
            .appendingPathComponent("wendy-device-info", isDirectory: true)
            .appendingPathComponent("prints-json-device-information", isDirectory: true)
            .appendingPathComponent("macos-to-rpi", isDirectory: true)
            .appendingPathComponent("0001", isDirectory: true)

        #expect(
            !FileManager.default.fileExists(
                atPath: aggregateObservationURL.appendingPathComponent("coverage").path
            )
        )
        for relative in [
            "coverage/bin", "coverage/bootstrap", "coverage/go-native.tar.gz",
            "coverage/coverage.out", "report",
        ] {
            #expect(
                !FileManager.default.fileExists(
                    atPath: attemptArtifactsURL.appendingPathComponent(relative).path
                )
            )
        }
        for name in ["source.json", "summary.json", "test-0.xml"] {
            let data = try String(
                contentsOf: attemptArtifactsURL.appendingPathComponent("coverage/\(name)"),
                encoding: .utf8
            )
            #expect(data == "provenance and failure evidence")
        }

        #expect(
            FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("attempt.json").path
            )
        )
        #expect(
            FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("test-results.xml").path
            )
        )
        #expect(
            FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("attempt.log").path
            )
        )
        #expect(
            !FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("observations").path
            )
        )
        #expect(
            FileManager.default.fileExists(
                atPath: aggregateObservationURL.appendingPathComponent("recording.md").path
            )
        )
        #expect(
            FileManager.default.fileExists(
                atPath: aggregateObservationURL.appendingPathComponent("test.json").path
            )
        )
        let aggregateTestRootURL = aggregateObservationURL.deletingLastPathComponent()
            .deletingLastPathComponent()
        #expect(
            FileManager.default.fileExists(
                atPath: aggregateTestRootURL.appendingPathComponent("test.json").path
            )
        )
        let sourceURL = aggregateTestRootURL.appendingPathComponent(e2eSourceArtifactFileName)
        #expect(FileManager.default.fileExists(atPath: sourceURL.path))
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        #expect(source.contains("Documents the expected JSON output."))
        #expect(source.contains("func `prints JSON device information`()"))
        let sourceIndexURL = runURL.appendingPathComponent(e2eSourceIndexFileName)
        #expect(FileManager.default.fileExists(atPath: sourceIndexURL.path))
        #expect(
            !FileManager.default.fileExists(
                atPath: aggregateObservationURL.appendingPathComponent("attempt.json").path
            )
        )
        #expect(
            !FileManager.default.fileExists(
                atPath: aggregateObservationURL.appendingPathComponent("test-results.xml").path
            )
        )
    }

    @Test
    func `preserves attempt artifacts when there are no observations`() throws {
        let rootURL = aggregateTemporaryDirectory()
        defer { try? FileManager.default.removeItem(at: rootURL) }

        let outputURL = rootURL.appendingPathComponent("aggregate", isDirectory: true)
        let attemptURL = rootURL.appendingPathComponent(
            "swift-e2e-tests.local0000.macos-to-rpi.0001",
            isDirectory: true
        )
        try FileManager.default.createDirectory(at: attemptURL, withIntermediateDirectories: true)
        try "{}\n".write(
            to: attemptURL.appendingPathComponent("attempt.json"),
            atomically: true,
            encoding: .utf8
        )
        try "<testsuite />\n".write(
            to: attemptURL.appendingPathComponent("test-results.xml"),
            atomically: true,
            encoding: .utf8
        )

        var command = try AggregateCommand.parse(["--output-dir", outputURL.path, attemptURL.path])
        try command.run()

        let runURL = outputURL.appendingPathComponent(
            "swift-e2e-tests.local0000",
            isDirectory: true
        )
        let attemptArtifactsURL =
            runURL
            .appendingPathComponent("attempts", isDirectory: true)
            .appendingPathComponent("macos-to-rpi", isDirectory: true)
            .appendingPathComponent("0001", isDirectory: true)

        #expect(
            FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("attempt.json").path
            )
        )
        #expect(
            FileManager.default.fileExists(
                atPath: attemptArtifactsURL.appendingPathComponent("test-results.xml").path
            )
        )
    }
}

private func writeAggregateTestMetadata(to observationURL: URL) throws {
    let metadata = E2ETestMetadata(
        schema: e2eTestMetadataSchemaID,
        sourceFilePath: "Tests/WendyE2ETests/WendyDeviceInfoTests.swift",
        sourceFileName: "WendyDeviceInfoTests",
        suiteName: "wendy device info",
        testName: "prints JSON device information",
        functionName: "`prints JSON device information`()",
        line: 10,
        declarationLine: 9,
        sourceStartLine: 5,
        sourceEndLine: 11
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    try encoder.encode(metadata).write(
        to: observationURL.appendingPathComponent(e2eTestMetadataFileName),
        options: .atomic
    )
}

private func writeAggregateTestSource(in packageURL: URL) throws {
    let sourceURL =
        packageURL
        .appendingPathComponent("Tests/WendyE2ETests", isDirectory: true)
        .appendingPathComponent("WendyDeviceInfoTests.swift")
    try FileManager.default.createDirectory(
        at: sourceURL.deletingLastPathComponent(),
        withIntermediateDirectories: true
    )
    try """
    import Testing

    @Suite
    struct `wendy device info` {
        /**
         Documents the expected JSON output.
         */
        @Test
        func `prints JSON device information`() async throws {
            #expect(true)
        }
    }

    """.write(to: sourceURL, atomically: true, encoding: .utf8)
}

private func aggregateTemporaryDirectory() -> URL {
    URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
        .appendingPathComponent("wendy-e2e-cli-tests-\(UUID().uuidString)", isDirectory: true)
}
