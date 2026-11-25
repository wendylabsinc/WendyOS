import ArgumentParser
import Foundation

// Coverage uses the CLI's existing Foundation process/JSON approach. Temporary
// files keep large Go metadata and stdin/stdout from blocking on full pipes.
enum CoverageIO {
    struct Output {
        var status: Int32
        var data: Data
        var error: String
        var text: String { String(decoding: data, as: UTF8.self) }
    }

    #if os(Windows)
        static let windowsExecutables = true
    #else
        static let windowsExecutables = false
    #endif

    /// Resolve native programs without a Unix launcher or shell interpretation.
    static func executable(
        _ name: String,
        environment: [String: String],
        cwd: URL? = nil,
        windows: Bool = windowsExecutables
    ) throws -> URL {
        guard !name.isEmpty else { throw ValidationError("Missing executable") }
        let directory = cwd ?? URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        var suffixes = [""]
        if windows && URL(fileURLWithPath: name).pathExtension.isEmpty {
            let extensions =
                environment["PATHEXT"] ?? environment.first(where: {
                    $0.key.lowercased() == "pathext"
                })?.value ?? ".COM;.EXE"
            suffixes +=
                extensions.split(separator: ";").map(String.init).filter {
                    [".exe", ".com"].contains($0.lowercased())
                } + [".exe", ".com"]
        }
        let explicitPath =
            name.contains("/") || (windows && (name.contains("\\") || name.contains(":")))
        let path =
            environment["PATH"] ?? environment.first(where: {
                windows && $0.key.lowercased() == "path"
            })?.value ?? (windows ? "" : "/usr/bin:/bin")
        let directories =
            explicitPath
            ? [directory]
            : path.split(separator: windows ? ";" : ":", omittingEmptySubsequences: false).map {
                URL(
                    fileURLWithPath: $0.isEmpty
                        ? "." : String($0).trimmingCharacters(in: CharacterSet(charactersIn: "\"")),
                    relativeTo: directory
                )
            }
        for root in directories {
            for suffix in suffixes {
                let candidate =
                    explicitPath
                    ? URL(fileURLWithPath: name + suffix, relativeTo: directory).standardizedFileURL
                    : root.appendingPathComponent(name + suffix).standardizedFileURL
                var isDirectory: ObjCBool = false
                guard
                    FileManager.default.fileExists(
                        atPath: candidate.path,
                        isDirectory: &isDirectory
                    ), !isDirectory.boolValue
                else { continue }
                if windows || FileManager.default.isExecutableFile(atPath: candidate.path) {
                    return candidate
                }
            }
        }
        throw ValidationError("Executable not found in PATH: " + name)
    }

    static func execute(
        _ arguments: [String],
        cwd: URL? = nil,
        input: Data? = nil,
        environment: [String: String]? = nil,
        inheritOutput: Bool = false
    ) throws -> Output {
        let temporary = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: temporary) }
        let stdout = temporary.appendingPathComponent("stdout")
        let stderr = temporary.appendingPathComponent("stderr")
        try Data().write(to: stdout)
        try Data().write(to: stderr)
        let out = try FileHandle(forWritingTo: stdout)
        let err = try FileHandle(forWritingTo: stderr)
        defer {
            try? out.close()
            try? err.close()
        }
        guard let program = arguments.first else { throw ValidationError("Missing executable") }
        let process = Process()
        let environment = environment ?? ProcessInfo.processInfo.environment
        process.executableURL = try executable(program, environment: environment, cwd: cwd)
        process.arguments = Array(arguments.dropFirst())
        process.currentDirectoryURL = cwd
        process.environment = environment
        if !inheritOutput {
            process.standardOutput = out
            process.standardError = err
        }
        if let input {
            let path = temporary.appendingPathComponent("stdin")
            try input.write(to: path)
            let handle = try FileHandle(forReadingFrom: path)
            defer { try? handle.close() }
            process.standardInput = handle
            try process.run()
            process.waitUntilExit()
        } else {
            process.standardInput = FileHandle.nullDevice
            try process.run()
            process.waitUntilExit()
        }
        return Output(
            status: process.terminationStatus,
            data: try Data(contentsOf: stdout),
            error: try String(contentsOf: stderr, encoding: .utf8)
        )
    }

    static func command(
        _ arguments: [String],
        cwd: URL? = nil,
        input: Data? = nil,
        environment: [String: String]? = nil
    ) throws -> String {
        let output = try execute(arguments, cwd: cwd, input: input, environment: environment)
        guard output.status == 0 else {
            throw ValidationError(
                "\(arguments.joined(separator: " ")) exited \(output.status): \(output.error)"
            )
        }
        return output.text
    }

    static func temporaryDirectory(in parent: URL? = nil) throws -> URL {
        let url = (parent ?? FileManager.default.temporaryDirectory)
            .appendingPathComponent("e2e-coverage-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    static func recordedRevision(_ metadata: [String: Any]) -> String? {
        (metadata["git"] as? [String: Any])?["commit"] as? String
            ?? (metadata["github"] as? [String: Any])?["sha"] as? String
    }

    static func read(_ url: URL) throws -> [String: Any] {
        guard
            let result = try JSONSerialization.jsonObject(with: Data(contentsOf: url))
                as? [String: Any]
        else {
            throw ValidationError("Expected JSON object: \(url.path)")
        }
        return result
    }

    static func write(_ value: Any, to url: URL) throws {
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        var data = try JSONSerialization.data(
            withJSONObject: value,
            options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        )
        data.append(10)
        try data.write(to: url, options: .atomic)
    }

    static func records(_ url: URL) throws -> [[String: Any]] {
        guard
            let records = try JSONSerialization.jsonObject(with: Data(contentsOf: url))
                as? [[String: Any]],
            records.allSatisfy({
                $0["test"] is String && $0["status"] is String && $0["sourceID"] is String
            })
        else {
            throw ValidationError("Invalid captured test records: " + url.path)
        }
        return records
    }
    static func evidence(_ relative: String, in root: URL) throws -> URL {
        guard !relative.hasPrefix("/"), !relative.split(separator: "/").contains("..") else {
            throw ValidationError("Evidence path outside attempt: " + relative)
        }
        let path = root.appendingPathComponent(relative)
        _ = try self.relative(path, to: root)
        return path
    }

    static func exists(_ url: URL) -> Bool { FileManager.default.fileExists(atPath: url.path) }
    static func remove(_ url: URL) throws {
        if exists(url) { try FileManager.default.removeItem(at: url) }
    }
    static func children(_ url: URL) throws -> [URL] {
        guard exists(url) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: url,
            includingPropertiesForKeys: [.isRegularFileKey]
        )
        .sorted { $0.path < $1.path }
    }
    static func files(_ url: URL, prefix: String = "") throws -> [URL] {
        try children(url).filter {
            try $0.lastPathComponent.hasPrefix(prefix)
                && $0.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile == true
        }
    }
    static func relative(_ url: URL, to root: URL) throws -> String {
        let path = url.resolvingSymlinksInPath().standardizedFileURL.path
        let base = root.resolvingSymlinksInPath().standardizedFileURL.path
        guard path.hasPrefix(base + "/") else {
            throw ValidationError("Evidence path outside attempt: \(path)")
        }
        return String(path.dropFirst(base.count + 1))
    }
    static func lines(_ text: String) -> [String] {
        var result = text.components(separatedBy: "\n")
        if result.last == "" { result.removeLast() }
        return result
    }
    static func matches(_ pattern: String, _ text: String) throws -> Bool {
        try NSRegularExpression(pattern: pattern).firstMatch(
            in: text,
            range: NSRange(text.startIndex..., in: text)
        ) != nil
    }
    static func sha256(_ data: Data) throws -> String {
        #if os(macOS)
            let args = ["shasum", "-a", "256"]
        #else
            let args = ["sha256sum"]
        #endif
        return try command(args, input: data).components(separatedBy: " ")[0]
    }
    static func sha256(file: URL) throws -> String { try sha256(Data(contentsOf: file)) }

    static func hashes(_ paths: [URL]) throws -> [String] {
        var result: [String] = []
        for offset in stride(from: 0, to: paths.count, by: 100) {
            let batch = Array(paths[offset..<min(offset + 100, paths.count)])
            #if os(macOS)
                let arguments = ["shasum", "-a", "256", "--"]
            #else
                let arguments = ["sha256sum", "--"]
            #endif
            let output = try command(arguments + batch.map(\.path))
            let digests = lines(output).map { String($0.drop(while: { $0 == "\\" }).prefix(64)) }
            guard digests.count == batch.count,
                digests.allSatisfy({ $0.count == 64 && $0.allSatisfy(\.isHexDigit) })
            else {
                throw ValidationError("Invalid SHA-256 tool output")
            }
            result += digests
        }
        return result
    }

    static func repository(_ path: String?) throws -> URL {
        if let path { return URL(fileURLWithPath: path).resolvingSymlinksInPath() }
        return URL(
            fileURLWithPath: try command(["git", "rev-parse", "--show-toplevel"])
                .trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }
    static func sourceID(repo: URL, paths: [String] = []) throws -> String {
        let tracked = try command(
            ["git", "ls-files", "-co", "--exclude-standard", "-z"],
            cwd: repo
        ).components(separatedBy: "\0")
        var data = Data()
        for name in Set(tracked + paths).sorted()
        where name.hasSuffix(".go") || ["go.mod", "go.sum"].contains(name) {
            data.append(contentsOf: name.utf8)
            data.append(0)
            let file = repo.appendingPathComponent(name)
            if exists(file) {
                data.append(contentsOf: "present\0".utf8)
                data.append(try Data(contentsOf: file))
            } else {
                data.append(contentsOf: "deleted".utf8)
            }
            data.append(0)
        }
        return try sha256(data)
    }
}
