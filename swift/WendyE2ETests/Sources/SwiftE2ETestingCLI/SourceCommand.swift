import ArgumentParser
import Foundation

/// Internal CI data preparation. Never builds or runs the tested checkout.
struct SourceCommand: ParsableCommand {
    static let configuration = CommandConfiguration(commandName: "_source", shouldDisplay: false)
    @Option var repo: String
    @Option var revision: String
    @Option var outputDir: String

    mutating func run() throws {
        try CoverageIO.materializeSource(
            repo: URL(fileURLWithPath: repo),
            revision: revision,
            into: URL(fileURLWithPath: outputDir)
        )
    }
}

extension CoverageIO {
    static func materializeSource(repo: URL, revision: String, into destination: URL) throws {
        guard revision.count == 40, revision.allSatisfy(\.isHexDigit), !exists(destination) else {
            throw ValidationError(
                "Source preparation requires an immutable SHA and a new destination"
            )
        }
        try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
        let git = destination.appendingPathComponent(".git")
        _ = try command([
            "git", "-c", "core.hooksPath=/dev/null", "clone", "--shared", "--mirror", repo.path,
            git.path,
        ])
        _ = try command(["git", "--git-dir", git.path, "config", "core.bare", "false"])
        _ = try command(["git", "-C", destination.path, "config", "core.hooksPath", "/dev/null"])
        // This private index/worktree is raw data. Prevent later inspection from
        // invoking global/local conversion drivers selected by tested attributes.
        try Data(
            "* -filter -text -ident -working-tree-encoding -export-subst -export-ignore\n".utf8
        )
        .write(to: git.appendingPathComponent("info/attributes"))
        _ = try command([
            "git", "-C", destination.path, "update-ref", "--no-deref", "HEAD", revision,
        ])
        _ = try command(["git", "-C", destination.path, "read-tree", revision])
        let tree = try execute(
            ["git", "ls-tree", "-rz", "--full-tree", revision],
            cwd: destination
        )
        guard tree.status == 0, let listing = String(data: tree.data, encoding: .utf8) else {
            throw ValidationError("Unable to read tested source tree")
        }
        var entries: [(path: String, hash: String, executable: Bool)] = []
        for row in listing.split(separator: "\0") {
            guard let tab = row.firstIndex(of: "\t") else {
                throw ValidationError("Invalid Git tree entry")
            }
            let fields = row[..<tab].split(separator: " ")
            let path = String(row[row.index(after: tab)...])
            let parts = path.split(separator: "/", omittingEmptySubsequences: false)
            guard fields.count == 3, !path.hasPrefix("/"), !path.contains("\\"),
                !path.contains(":"),
                !path.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }
                ),
                parts.allSatisfy({
                    !$0.isEmpty && $0 != "." && $0 != ".." && $0.lowercased() != ".git"
                })
            else { throw ValidationError("Unsafe tested source path") }
            if fields[0] == "120000" || fields[0] == "160000" { continue }  // no symlink or submodule traversal
            guard ["100644", "100755"].contains(fields[0]), fields[1] == "blob",
                fields[2].count == 40, fields[2].allSatisfy(\.isHexDigit)
            else { throw ValidationError("Unsupported tested source mode") }
            entries.append((path, String(fields[2]), fields[0] == "100755"))
        }
        try readSourceBlobs(entries.map(\.hash), in: destination) { index, contents in
            let entry = entries[index]
            let file = destination.appendingPathComponent(entry.path)
            try FileManager.default.createDirectory(
                at: file.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            try contents.write(to: file)
            try FileManager.default.setAttributes(
                [.posixPermissions: entry.executable ? 0o555 : 0o444],
                ofItemAtPath: file.path
            )
        }
        _ = try command(["chmod", "-R", "a-w", destination.path])
    }

    static func readSourceBlobs(
        _ hashes: [String],
        in repo: URL,
        consume: (Int, Data) throws -> Void
    ) throws {
        // Batch raw blobs; neither attributes, checkout filters nor hooks run.
        for offset in stride(from: 0, to: hashes.count, by: 100) {
            let batch = hashes[offset..<min(hashes.count, offset + 100)]
            let result = try execute(
                ["git", "cat-file", "--batch"],
                cwd: repo,
                input: Data(batch.map { $0 + "\n" }.joined().utf8)
            )
            guard result.status == 0 else {
                throw ValidationError("Unable to read raw source blobs: " + result.error)
            }
            let bytes = [UInt8](result.data)
            var cursor = 0
            for index in batch.indices {
                guard let end = bytes[cursor...].firstIndex(of: 10) else {
                    throw ValidationError("Invalid raw blob header")
                }
                let header = String(decoding: bytes[cursor..<end], as: UTF8.self).split(
                    separator: " "
                )
                guard header.count == 3, header[0] == hashes[index], header[1] == "blob",
                    let size = Int(header[2]), size >= 0, size < bytes.count - end - 1
                else { throw ValidationError("Invalid raw source blob") }
                cursor = end + 1
                guard bytes[cursor + size] == 10 else {
                    throw ValidationError("Invalid raw blob terminator")
                }
                try consume(index, Data(bytes[cursor..<(cursor + size)]))
                cursor += size + 1
            }
            guard cursor == bytes.count else {
                throw ValidationError("Unexpected raw source bytes")
            }
        }
    }
}
