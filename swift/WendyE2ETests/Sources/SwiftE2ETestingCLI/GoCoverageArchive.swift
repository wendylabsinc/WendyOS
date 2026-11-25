import ArgumentParser
import Foundation

extension GoCoverage {
    static func nativeFiles(run: URL) throws -> [URL] {
        var result: [URL] = []
        for suite in try CoverageIO.children(run.appendingPathComponent("observations")) {
            for test in try CoverageIO.children(suite) {
                result += try CoverageIO.files(test.appendingPathComponent("coverage/go"))
            }
        }
        return result.sorted { $0.path < $1.path }
            + (try CoverageIO.files(run.appendingPathComponent("coverage/bootstrap")))
    }

    static func pack(run input: URL) throws {
        let run = input.resolvingSymlinksInPath()
        guard CoverageIO.exists(run) else {
            throw ValidationError("Attempt directory does not exist: \(run.path)")
        }
        let paths = try nativeFiles(run: run)
        let archive = run.appendingPathComponent("coverage/go-native.tar.gz")
        if !paths.isEmpty {
            guard !CoverageIO.exists(archive) else {
                throw ValidationError("Native archive already exists; refusing to overwrite it")
            }
            try FileManager.default.createDirectory(
                at: archive.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            let temporary = archive.deletingPathExtension().appendingPathExtension("partial")
            let staging = try CoverageIO.temporaryDirectory(in: archive.deletingLastPathComponent())
            defer { try? FileManager.default.removeItem(at: staging) }
            let contents = staging.appendingPathComponent("contents")
            let recovered = staging.appendingPathComponent("recovered")
            try FileManager.default.createDirectory(
                at: recovered,
                withIntermediateDirectories: true
            )
            let digests = try CoverageIO.hashes(paths)
            let names = try paths.map { try CoverageIO.relative($0, to: run) }
            var metadata: [String: URL] = [:]
            for (index, path) in paths.enumerated() {
                let destination = contents.appendingPathComponent(names[index])
                try FileManager.default.createDirectory(
                    at: destination.deletingLastPathComponent(),
                    withIntermediateDirectories: true
                )
                // Let standard tar encode hard links, without changing originals.
                let original =
                    path.lastPathComponent.hasPrefix("covmeta.")
                    ? metadata[digests[index]] ?? path : path
                try FileManager.default.linkItem(at: original, to: destination)
                if path.lastPathComponent.hasPrefix("covmeta.") {
                    metadata[digests[index]] = destination
                }
            }
            let list = staging.appendingPathComponent("paths")
            try Data((names.joined(separator: "\0") + "\0").utf8).write(to: list)
            var environment = ProcessInfo.processInfo.environment
            // Native Go data has no resource forks. macOS tar otherwise adds
            // AppleDouble ._ entries that are not in the native manifest.
            environment["COPYFILE_DISABLE"] = "1"
            _ = try CoverageIO.command(
                [
                    "tar", "-czf", temporary.path, "-C", contents.path, "--null", "-T", list.path,
                ],
                environment: environment
            )
            _ = try CoverageIO.command(["tar", "-xzf", temporary.path, "-C", recovered.path])
            guard
                try CoverageIO.hashes(names.map { recovered.appendingPathComponent($0) }) == digests
            else {
                throw ValidationError("Native archive verification failed")
            }
            try FileManager.default.moveItem(at: temporary, to: archive)
            try (zip(names, digests).map { "\($0.1)  \($0.0)\n" }.joined())
                .write(
                    to: run.appendingPathComponent("coverage/native.sha256"),
                    atomically: true,
                    encoding: .utf8
                )
            for path in paths { try FileManager.default.removeItem(at: path) }
            let bytes = try archive.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
            print("Packed \(paths.count) native files into \(bytes) bytes: \(archive.path)")
        }
        let bin = run.appendingPathComponent("coverage/bin")
        if CoverageIO.exists(bin) {
            let sourceURL = run.appendingPathComponent("coverage/source.json")
            var source = try CoverageIO.exists(sourceURL) ? CoverageIO.read(sourceURL) : [:]
            var binaries = source["binaries"] as? [String: Any] ?? [:]
            var changed = false
            for binary in try CoverageIO.files(bin) where binaries[binary.lastPathComponent] == nil
            {
                let info = try CoverageIO.execute(["go", "version", "-m", binary.path])
                binaries[binary.lastPathComponent] = [
                    "sha256": try CoverageIO.sha256(file: binary), "buildInfo": info.text,
                    "buildInfoError": info.error,
                ]
                changed = true
            }
            if changed {
                source["binaries"] = binaries
                try CoverageIO.write(source, to: sourceURL)
            }
            try CoverageIO.remove(bin)
        }
        for suite in try CoverageIO.children(run.appendingPathComponent("observations")) {
            for test in try CoverageIO.children(suite) {
                try CoverageIO.remove(test.appendingPathComponent("coverage/coverage.out"))
            }
        }
        for obsolete in ["coverage/diff.patch", "coverage/diff.patch.gz", "coverage/diff.json"] {
            try CoverageIO.remove(run.appendingPathComponent(obsolete))
        }
    }
}
