import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct `coverage native process resolution` {
    @Test func `Windows lookup honors Path PATHEXT spaces and explicit executable paths`() throws {
        let root = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let bin = root.appendingPathComponent("bin with spaces")
        try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
        let git = bin.appendingPathComponent("git.EXE")
        try Data("native executable fixture".utf8).write(to: git)
        let environment = [
            "Path": root.path + ";\"" + bin.path + "\"", "PATHEXT": ".COM;.EXE;.BAT;.CMD",
        ]
        #expect(try CoverageIO.executable("git", environment: environment, windows: true) == git)
        #expect(try CoverageIO.executable(git.path, environment: [:], windows: true) == git)
        #expect(throws: (any Error).self) {
            try CoverageIO.executable("missing", environment: environment, windows: true)
        }
        #expect(throws: (any Error).self) { try CoverageIO.execute([]) }
    }

    @Test func `direct native execution preserves stdin output and nonzero status`() throws {
        let hash = try CoverageIO.execute(
            ["git", "hash-object", "--stdin"],
            input: Data("hello\n".utf8)
        )
        #expect(hash.status == 0)
        #expect(hash.text == "ce013625030ba8dba906f756967f9e9ca394464a\n")
        #expect(hash.error.isEmpty)
        let root = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let failed = try CoverageIO.execute(
            ["git", "rev-parse", "--verify", "not-a-revision"],
            cwd: root
        )
        #expect(failed.status != 0)
        #expect(!failed.error.isEmpty)
    }
}
