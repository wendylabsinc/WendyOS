import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct `reference command` {
    @Test
    func `validates every source before writing output files`() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let valid = root.appendingPathComponent("valid.swift")
        let invalid = root.appendingPathComponent("invalid.swift")
        try """
        @Suite
        struct `valid` {
            @Test
            func `first`() {}
        }
        """.write(to: valid, atomically: true, encoding: .utf8)
        try """
        @Suite
        struct `invalid` {
            @Test(.disabled("reason")
            func `lost`() {}
        }
        """.write(to: invalid, atomically: true, encoding: .utf8)
        for format in ["markdown", "html", "json"] {
            let output = root.appendingPathComponent(format)
            var command = try ReferenceCommand.parse([
                "--format", format, "--output", output.path, valid.path, invalid.path,
            ])
            #expect(throws: (any Error).self) { try command.run() }
            #expect(!FileManager.default.fileExists(atPath: output.path))
        }
    }
}
