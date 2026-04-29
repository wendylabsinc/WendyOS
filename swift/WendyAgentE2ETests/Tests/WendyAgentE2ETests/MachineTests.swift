import Foundation
import Testing

@testable import WendyAgentE2E

struct MachineTests {
    @Test("parse local machine spec")
    func parseLocalMachineSpec() throws {
        let machine = try Machine.parse("local:~/wendy-agent")

        #expect(machine.isLocal)
        #expect(machine.sshTarget == nil)
        #expect(machine.baseDirectory.hasSuffix("/wendy-agent"))
    }

    @Test("parse ssh machine spec")
    func parseSSHMachineSpec() throws {
        let machine = try Machine.parse("ai@example.local:~/wendy-agent")

        #expect(machine.isLocal == false)
        #expect(machine.sshTarget == "ai@example.local")
        #expect(machine.baseDirectory == "~/wendy-agent")
    }

    @Test("local machine can run shell commands")
    func localMachineCanRunShellCommands() async throws {
        let workspace = try TemporaryDirectory(prefix: "machine-run-")
        defer { workspace.remove() }

        let machine = Machine.local(workspace.path)

        try await machine.run("touch ok.txt")
        #expect(FileManager.default.fileExists(atPath: workspace.path + "/ok.txt"))
    }

    @Test("local push copies the trailing path component into the destination base directory")
    func localPushCopiesTrailingPathComponent() async throws {
        let sourceWorkspace = try TemporaryDirectory(prefix: "machine-source-")
        let destinationWorkspace = try TemporaryDirectory(prefix: "machine-destination-")
        defer {
            sourceWorkspace.remove()
            destinationWorkspace.remove()
        }

        let nestedDirectory = URL(fileURLWithPath: sourceWorkspace.path)
            .appendingPathComponent("nested", isDirectory: true)
        try FileManager.default.createDirectory(
            at: nestedDirectory,
            withIntermediateDirectories: true
        )

        let sourceFile = nestedDirectory.appendingPathComponent("tool.txt")
        try "hello".write(to: sourceFile, atomically: true, encoding: .utf8)

        let source = Machine.local(sourceWorkspace.path)
        let destination = Machine.local(destinationWorkspace.path)

        try await source.push("nested/tool.txt", to: destination)

        let copiedFile = URL(fileURLWithPath: destinationWorkspace.path)
            .appendingPathComponent("tool.txt")
        let contents = try String(contentsOf: copiedFile, encoding: .utf8)
        #expect(contents == "hello")
    }
}

private struct TemporaryDirectory {
    let url: URL

    init(prefix: String) throws {
        let root = FileManager.default.temporaryDirectory
        self.url = root.appendingPathComponent(prefix + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: self.url, withIntermediateDirectories: true)
    }

    var path: String {
        self.url.path
    }

    func remove() {
        try? FileManager.default.removeItem(at: self.url)
    }
}
