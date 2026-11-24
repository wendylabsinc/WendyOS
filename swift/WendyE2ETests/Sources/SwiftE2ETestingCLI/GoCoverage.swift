import ArgumentParser
import Foundation

#if canImport(FoundationXML)
    import FoundationXML
#endif
#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#endif

struct GoCoverage {
    let repo: URL
    let run: URL
    init(repo: URL, run: URL) {
        self.repo = repo.resolvingSymlinksInPath()
        self.run = run.resolvingSymlinksInPath()
    }
    static let hookPaths: Set<String> = [
        "go/cmd/wendy/coverage_e2e.go", "go/cmd/wendy-agent/coverage_e2e.go",
    ]
    var coverage: URL { run.appendingPathComponent("coverage") }

    func prepare(managed: Bool) throws {
        var files: [String: Any] = [:]
        var packages: [String: [String]] = [:]
        for role in managed ? ["wendy", "wendy-agent"] : ["wendy"] {
            let output = try CoverageIO.command(
                ["go", "list", "-deps", "-json", "-tags=e2e_coverage", "./go/cmd/" + role],
                cwd: repo
            )
            var entries: [String] = []
            // go list emits consecutive pretty-printed root objects. Its root
            // closing brace is unindented; string newlines are JSON-escaped.
            var entriesJSON: [[String: Any]] = []
            var packet = ""
            for line in CoverageIO.lines(output) {
                packet += line + "\n"
                if line == "}" {
                    guard
                        let entry = try JSONSerialization.jsonObject(with: Data(packet.utf8))
                            as? [String: Any]
                    else { throw ValidationError("Invalid go list package") }
                    entriesJSON.append(entry)
                    packet = ""
                }
            }
            guard packet.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                throw ValidationError("Truncated go list output")
            }
            for pkg in entriesJSON {
                guard (pkg["Module"] as? [String: Any])?["Main"] as? Bool == true else { continue }
                let name = pkg["ImportPath"] as! String
                entries.append(name)
                for filename in (pkg["GoFiles"] as? [String] ?? [])
                    + (pkg["CgoFiles"] as? [String] ?? [])
                {
                    let path = URL(fileURLWithPath: pkg["Dir"] as! String).appendingPathComponent(
                        filename
                    )
                    let text = try String(contentsOf: path, encoding: .utf8)
                    files[name + "/" + filename] = [
                        "path": try CoverageIO.relative(path, to: repo),
                        "generated": try Self.generated(text),
                        "testHarness": Self.hookPaths.contains(
                            try CoverageIO.relative(path, to: repo)
                        ),
                    ]
                }
            }
            packages[role] = entries.sorted()
        }
        let scopedPaths = Self.sourcePaths(files)
        let attributes = try CoverageIO.command(
            ["git", "check-attr", "-z", "--stdin", "linguist-generated"],
            cwd: repo,
            input: Data((scopedPaths.joined(separator: "\0") + "\0").utf8)
        ).components(separatedBy: "\0")
        var generatedPaths = Set<String>()
        for index in stride(from: 0, to: max(0, attributes.count - 2), by: 3) {
            if ["true", "set"].contains(attributes[index + 2]) {
                generatedPaths.insert(attributes[index])
            }
        }
        for key in files.keys {
            var file = files[key] as! [String: Any]
            if generatedPaths.contains(file["path"] as! String) {
                file["generated"] = true
                files[key] = file
            }
        }
        let source: [String: Any] = [
            "sourceID": try CoverageIO.sourceID(repo: repo, paths: Self.sourcePaths(files)),
            "revision": try CoverageIO.command(["git", "rev-parse", "HEAD"], cwd: repo)
                .trimmingCharacters(in: .whitespacesAndNewlines),
            "goVersion": try CoverageIO.command(["go", "version"]).trimmingCharacters(
                in: .whitespacesAndNewlines
            ),
            "files": files, "packages": packages,
            "scope":
                "Go main-module packages linked into the selected binaries on this platform; generated files excluded only from derived reports",
            "agent": managed ? "instrumented managed Go Agent" : "uninstrumented/not collected",
            "counterMode": "atomic", "nonGo": "out of scope",
        ]
        try CoverageIO.write(source, to: coverage.appendingPathComponent("source.json"))
    }

    static func generated(_ text: String) throws -> Bool {
        let prefix = CoverageIO.lines(text).prefix {
            $0.split(whereSeparator: \.isWhitespace).first != "package"
        }.joined(separator: "\n")
        return try CoverageIO.matches(#"(?m)^// Code generated .* DO NOT EDIT\.$"#, prefix)
    }
    static func validateSource(_ source: [String: Any]) throws {
        guard let identity = source["sourceID"] as? String, identity.count == 64,
            identity.allSatisfy(\.isHexDigit),
            let revision = source["revision"] as? String, revision.count == 40,
            revision.allSatisfy(\.isHexDigit),
            let files = source["files"] as? [String: [String: Any]],
            let packages = source["packages"] as? [String: [String]], !packages.isEmpty,
            source["agent"] is String, source["scope"] is String,
            source["counterMode"] as? String == "atomic"
        else { throw ValidationError("Invalid Go coverage source provenance") }
        for file in files.values {
            guard let path = file["path"] as? String, !path.hasPrefix("/"),
                !path.split(separator: "/").contains(".."), file["generated"] is Bool,
                file["testHarness"] is Bool
            else {
                throw ValidationError("Invalid Go coverage source file scope")
            }
        }
    }

    static func sourcePaths(_ files: [String: Any]) -> [String] {
        files.values.compactMap { ($0 as? [String: Any])?["path"] as? String }
    }

    static func validateNative(_ natives: [URL]) throws {
        for native in natives {
            let metadata = Set(
                try CoverageIO.files(native, prefix: "covmeta.").map {
                    String($0.lastPathComponent.dropFirst(8))
                }
            )
            let counters = Set(
                try CoverageIO.files(native, prefix: "covcounters.").map {
                    $0.lastPathComponent.components(separatedBy: ".")[1]
                }
            )
            guard !metadata.isEmpty && !counters.isEmpty else {
                throw ValidationError("Missing native Go metadata/counters: " + native.path)
            }
            guard counters.isSubset(of: metadata) else {
                throw ValidationError("Missing native Go metadata for counters: " + native.path)
            }
            guard metadata.isSubset(of: counters) else {
                throw ValidationError("Missing native Go counters for metadata: " + native.path)
            }
        }
    }

    static func profile(
        _ natives: [URL],
        destination: URL,
        source: [String: Any],
        writeProfile: Bool = true
    ) throws -> [String: Any] {
        try validateNative(natives)
        try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
        let merged = try CoverageIO.temporaryDirectory(in: destination)
        defer { try? FileManager.default.removeItem(at: merged) }
        var inputs = natives
        if natives.count > 100 {
            inputs = []
            for start in stride(from: 0, to: natives.count, by: 100) {
                let part = merged.appendingPathComponent("part-\(start)")
                try FileManager.default.createDirectory(at: part, withIntermediateDirectories: true)
                let paths = natives[start..<min(start + 100, natives.count)].map(\.path).joined(
                    separator: ","
                )
                _ = try CoverageIO.command([
                    "go", "tool", "covdata", "merge", "-pcombine", "-i=" + paths, "-o=" + part.path,
                ])
                inputs.append(part)
            }
        }
        let union = merged.appendingPathComponent("union")
        try FileManager.default.createDirectory(at: union, withIntermediateDirectories: true)
        _ = try CoverageIO.command([
            "go", "tool", "covdata", "merge", "-pcombine",
            "-i=" + inputs.map(\.path).joined(separator: ","), "-o=" + union.path,
        ])
        let profile = merged.appendingPathComponent("coverage.out")
        _ = try CoverageIO.command([
            "go", "tool", "covdata", "textfmt", "-i=" + union.path, "-o=" + profile.path,
        ])
        let raw = try String(contentsOf: profile, encoding: .utf8)
        guard raw.hasPrefix("mode: atomic\n") else {
            throw ValidationError("Expected atomic native Go coverage")
        }
        let files = source["files"] as! [String: [String: Any]]
        let known = Set(files.keys)
        let excluded = Set(
            files.filter {
                $0.value["generated"] as? Bool == true || $0.value["testHarness"] as? Bool == true
            }.keys
        )
        var lines = ["mode: atomic"]
        var totals: [String: [String: Int]] = [:]
        for line in CoverageIO.lines(raw).dropFirst() {
            let block = try GoCoverageBlock(line)
            guard known.contains(block.file) else {
                throw ValidationError("Coverage source outside recorded build scope: " + block.file)
            }
            if excluded.contains(block.file) { continue }
            lines.append(line)
            guard let separator = block.file.lastIndex(of: "/") else {
                throw ValidationError("Invalid Go coverage package path")
            }
            let package = String(block.file[..<separator])
            totals[package, default: ["statements": 0, "covered": 0]]["statements", default: 0] +=
                block.statements
            totals[package, default: ["statements": 0, "covered": 0]]["covered", default: 0] +=
                block.count > 0 ? block.statements : 0
        }
        if writeProfile {
            try (lines.joined(separator: "\n") + "\n").write(
                to: destination.appendingPathComponent("coverage.out"),
                atomically: true,
                encoding: .utf8
            )
        }
        let statements = totals.values.reduce(0) { $0 + $1["statements", default: 0] }
        let covered = totals.values.reduce(0) { $0 + $1["covered", default: 0] }
        return [
            "statements": statements, "covered": covered,
            "percent": statements > 0
                ? 100.0 * Double(covered) / Double(statements) as Any : NSNull(),
            "packages": totals,
        ]
    }

    static func checkCLICounters(_ native: URL) throws -> Int {
        let counters = try CoverageIO.files(native, prefix: "covcounters.")
        let starts = try CoverageIO.files(native, prefix: "invocation.")
        for start in starts {
            let fields = start.lastPathComponent.components(separatedBy: ".")
            guard fields.count == 3, let timestamp = Int64(fields[2]),
                counters.contains(where: {
                    let parts = $0.lastPathComponent.components(separatedBy: ".")
                    return parts.count == 4 && parts[2] == fields[1]
                        && (Int64(parts[3]) ?? -1) >= timestamp
                })
            else {
                throw ValidationError(
                    "CLI invocation did not write counters: " + start.lastPathComponent
                )
            }
        }
        if try !starts.isEmpty
            && (counters.isEmpty || CoverageIO.files(native, prefix: "covmeta.").isEmpty)
        {
            throw ValidationError("Missing native Go counters or metadata")
        }
        return starts.count
    }

    static func runTests(
        run input: URL,
        filters: [String],
        control: String?,
        environment: [String: String] = ProcessInfo.processInfo.environment
    ) throws -> Int32 {
        let run = input.resolvingSymlinksInPath()
        let coverage = run.appendingPathComponent("coverage")
        var source = try CoverageIO.read(coverage.appendingPathComponent("source.json"))
        try Self.validateSource(source)
        if (source["packages"] as? [String: Any])?["wendy-agent"] != nil && control == nil {
            throw ValidationError(
                "Instrumented managed Agent requires its private coverage control"
            )
        }
        var binaries: [String: Any] = [:]
        for role in (source["packages"] as! [String: Any]).keys.sorted() {
            let binary = coverage.appendingPathComponent("bin/" + role)
            let info = try CoverageIO.command(["go", "version", "-m", binary.path])
            guard info.contains("-cover=true") && info.contains("e2e_coverage") else {
                throw ValidationError("Expected instrumented Go binary: " + binary.path)
            }
            binaries[role] = ["sha256": try CoverageIO.sha256(file: binary), "buildInfo": info]
        }
        source["binaries"] = binaries
        try CoverageIO.write(source, to: coverage.appendingPathComponent("source.json"))
        var records: [[String: Any]] = []
        try CoverageIO.write(records, to: coverage.appendingPathComponent("tests.json"))
        let listed = try CoverageIO.execute(["swift", "test", "list"], environment: environment)
        FileHandle.standardError.write(Data(listed.error.utf8))
        guard listed.status == 0 else {
            throw ValidationError("swift test list exited \(listed.status): \(listed.error)")
        }
        let patterns = try filters.map { try NSRegularExpression(pattern: $0) }
        let tests = CoverageIO.lines(listed.text).filter { name in
            name.contains("/")
                && patterns.contains {
                    $0.firstMatch(in: name, range: NSRange(name.startIndex..., in: name)) != nil
                }
        }
        let combined = XMLElement(name: "testsuites")
        func save() throws {
            try CoverageIO.write(records, to: coverage.appendingPathComponent("tests.json"))
            let document = XMLDocument(rootElement: (combined.copy() as! XMLElement))
            document.version = "1.0"
            document.characterEncoding = "utf-8"
            try document.xmlData.write(
                to: run.appendingPathComponent("test-results.xml"),
                options: .atomic
            )
        }
        try save()
        var status: Int32 = 0
        for (index, name) in tests.enumerated() {
            let receipt = coverage.appendingPathComponent("current-test.txt")
            try CoverageIO.remove(receipt)
            let xml = coverage.appendingPathComponent("test-\(index).xml")
            var record: [String: Any] = [
                "test": name, "sourceID": source["sourceID"]!, "status": "incomplete",
            ]
            records.append(record)
            try save()
            do {
                if let control {
                    record["agentPID"] = try agentControl(control, operation: "reset")
                }
                var env = environment
                env["WENDY_E2E_COVERAGE_RECEIPT"] = receipt.path
                let result = try CoverageIO.execute(
                    [
                        "swift", "test", "--skip-build", "--no-parallel", "--filter",
                        "^" + NSRegularExpression.escapedPattern(for: name) + "(/|$)",
                        "--xunit-output", xml.path,
                    ],
                    environment: env,
                    inheritOutput: true
                )
                record["testExitStatus"] = Int(result.status)
                if status == 0 { status = result.status }
                var native: URL?
                if CoverageIO.exists(receipt) {
                    let test = URL(
                        fileURLWithPath: try String(contentsOf: receipt, encoding: .utf8)
                    ).resolvingSymlinksInPath()
                    native = test.appendingPathComponent("coverage/go")
                    record["directory"] = try CoverageIO.relative(test, to: run)
                    if let control {
                        guard
                            try agentControl(control, operation: "export", directory: native!.path)
                                == record["agentPID"] as? Int
                        else { throw ValidationError("Agent changed between reset and export") }
                        record["agentExported"] = true
                    }
                    records[index] = record
                    try save()
                }
                let swiftPath = coverage.appendingPathComponent("test-\(index)-swift-testing.xml")
                let xmlData = try Data(contentsOf: CoverageIO.exists(swiftPath) ? swiftPath : xml)
                let parser = XMLParser(data: xmlData)
                guard parser.parse(), parser.parserError == nil else {
                    throw ValidationError("Selected test produced invalid xUnit output")
                }
                let document = try XMLDocument(data: xmlData)
                let cases = try document.nodes(forXPath: "//testcase")
                guard !cases.isEmpty, let root = document.rootElement() else {
                    throw ValidationError("Selected test produced no xUnit cases")
                }
                let suites =
                    root.name == "testsuite"
                    ? [root] : (root.children ?? []).compactMap { $0 as? XMLElement }
                for suite in suites { combined.addChild(suite.copy() as! XMLNode) }
                if try cases.allSatisfy({ try !$0.nodes(forXPath: "skipped").isEmpty }) {
                    record["status"] = "skipped"
                } else if let native {
                    let count = try checkCLICounters(native)
                    record["cliInvocations"] = count
                    record["status"] = count > 0 || control != nil ? "collected" : "no-data"
                    record["cli"] = count > 0 ? "collected" : "not invoked"
                } else {
                    record["status"] = "no-data"
                    record["cli"] = "not invoked through the scenario harness"
                }
            } catch {
                record["error"] = String(describing: error)
                FileHandle.standardError.write(Data("ERROR: E2E coverage: \(name) \(error)\n".utf8))
                if status == 0 { status = 1 }
            }
            records[index] = record
            try save()
        }
        return status
    }

    func finishCapture(control: String?) throws -> Int32 {
        let source = try CoverageIO.read(coverage.appendingPathComponent("source.json"))
        try Self.validateSource(source)
        var records = try CoverageIO.records(coverage.appendingPathComponent("tests.json"))
        var errors: [String] = []
        if CoverageIO.exists(run.appendingPathComponent("timeout.json")) {
            errors.append("Test process timed out; coverage intervals may be incomplete")
        }
        if let index = records.indices.last, records[index]["status"] as? String == "incomplete" {
            let receipt = coverage.appendingPathComponent("current-test.txt")
            if CoverageIO.exists(receipt) {
                do {
                    let test = URL(
                        fileURLWithPath: try String(contentsOf: receipt, encoding: .utf8)
                    ).resolvingSymlinksInPath()
                    records[index]["directory"] = try CoverageIO.relative(test, to: run)
                    if let control, records[index]["agentExported"] as? Bool != true {
                        _ = try Self.agentControl(
                            control,
                            operation: "export",
                            directory: test.appendingPathComponent("coverage/go").path
                        )
                        records[index]["agentExported"] = true
                    }
                } catch { errors.append(String(describing: error)) }
            }
        }
        for i in records.indices {
            if ["skipped", "no-data"].contains(records[i]["status"] as? String ?? "") { continue }
            do {
                guard records[i]["status"] as? String == "collected",
                    let directory = records[i]["directory"] as? String
                else {
                    throw ValidationError(
                        records[i]["error"] as? String
                            ?? "Incomplete collection: \(records[i]["test"] ?? "")"
                    )
                }
                let native = try CoverageIO.evidence(directory + "/coverage/go", in: run)
                _ = try Self.checkCLICounters(native)
                try Self.validateNative([native])
            } catch {
                errors.append(String(describing: error))
                records[i]["status"] = "incomplete"
                records[i]["error"] = String(describing: error)
            }
        }
        try CoverageIO.write(records, to: coverage.appendingPathComponent("tests.json"))
        let attempt = run.appendingPathComponent("attempt.json")
        if CoverageIO.exists(attempt) {
            var info = try CoverageIO.read(attempt)
            info["coverage"] = [
                "status": errors.isEmpty ? "captured" : "incomplete",
                "sourceID": source["sourceID"]!, "agent": source["agent"]!, "errors": errors,
            ]
            if !errors.isEmpty && info["exitStatus"] as? Int == 0 { info["exitStatus"] = 1 }
            try CoverageIO.write(info, to: attempt)
        }
        for error in errors {
            FileHandle.standardError.write(Data("ERROR: E2E coverage: \(error)\n".utf8))
        }
        return errors.isEmpty ? 0 : 1
    }

    func deriveAttempt(perTest: Bool = false) throws -> Int32 {
        let archive = coverage.appendingPathComponent("go-native.tar.gz")
        if CoverageIO.exists(archive) {
            let members = CoverageIO.lines(try CoverageIO.command(["tar", "-tzf", archive.path]))
            if members.contains(where: { !CoverageIO.exists(run.appendingPathComponent($0)) }) {
                throw ValidationError(
                    "Native evidence is archived. Unpack in the attempt directory: tar -xzf coverage/go-native.tar.gz"
                )
            }
        }
        let source = try CoverageIO.read(coverage.appendingPathComponent("source.json"))
        try Self.validateSource(source)
        var records = try CoverageIO.records(coverage.appendingPathComponent("tests.json"))
        var errors: [String] = []
        var natives: [URL] = []
        try CoverageIO.remove(coverage.appendingPathComponent("coverage.out"))
        if CoverageIO.exists(run.appendingPathComponent("timeout.json")) {
            errors.append("Test process timed out; coverage intervals may be incomplete")
        }
        let compatible =
            try CoverageIO.sourceID(
                repo: repo,
                paths: Self.sourcePaths(source["files"] as! [String: Any])
            ) == source["sourceID"] as? String
        if !compatible {
            errors.append("Source changed since coverage build; refusing to mix revisions")
        }
        for index in records.indices {
            var record = records[index]
            guard record["sourceID"] as? String == source["sourceID"] as? String else {
                errors.append("Incompatible source identity: " + (record["test"] as? String ?? ""))
                continue
            }
            let state = record["status"] as? String
            if state == "skipped" || state == "no-data" {
                if let directory = record["directory"] as? String {
                    let path = try CoverageIO.evidence(directory + "/test.json", in: run)
                    var metadata = try CoverageIO.exists(path) ? CoverageIO.read(path) : [:]
                    var detail = record
                    detail["agent"] = source["agent"]
                    metadata["coverage"] = detail
                    try CoverageIO.write(metadata, to: path)
                }
                continue
            }
            if state != "collected" {
                errors.append(
                    record["error"] as? String ?? "Incomplete collection: \(record["test"] ?? "")"
                )
            }
            guard let directory = record["directory"] as? String else { continue }
            let test = try CoverageIO.evidence(directory, in: run)
            let native = test.appendingPathComponent("coverage/go")
            try CoverageIO.remove(test.appendingPathComponent("coverage/coverage.out"))
            var summary: [String: Any]?
            do {
                guard compatible else { throw ValidationError("Incompatible coverage source") }
                if state == "collected",
                    (source["packages"] as? [String: Any])?["wendy-agent"] != nil
                {
                    guard record["agentPID"] is Int, record["agentExported"] as? Bool == true else {
                        throw ValidationError(
                            "Missing managed Agent participant: \(record["test"] ?? "")"
                        )
                    }
                }
                let invocations = try Self.checkCLICounters(native)
                if state == "collected" {
                    guard let expected = record["cliInvocations"] as? Int, expected == invocations
                    else {
                        throw ValidationError(
                            "CLI invocation evidence changed since capture: \(record["test"] ?? "")"
                        )
                    }
                }
                if let pid = record["agentPID"] as? Int,
                    try !CoverageIO.files(native, prefix: "covcounters.").contains(where: {
                        $0.lastPathComponent.components(separatedBy: ".").dropFirst(2).first
                            == String(pid)
                    })
                {
                    throw ValidationError("Missing managed Agent counters: \(record["test"] ?? "")")
                }
                try Self.validateNative([native])
                record["nativeValidated"] = true
                if perTest {
                    summary = try Self.profile(
                        [native],
                        destination: test.appendingPathComponent("coverage"),
                        source: source
                    )
                }
                natives.append(native)
            } catch {
                errors.append(String(describing: error))
                record["status"] = "incomplete"
                record["error"] = String(describing: error)
                record["nativeValidated"] = false
            }
            let path = test.appendingPathComponent("test.json")
            var metadata = try CoverageIO.exists(path) ? CoverageIO.read(path) : [:]
            var detail = record
            if let summary { detail["summary"] = summary }
            detail["agent"] = source["agent"]
            metadata["coverage"] = detail
            try CoverageIO.write(metadata, to: path)
            records[index] = record
        }
        var result: [String: Any] = [
            "sourceID": source["sourceID"]!, "agent": source["agent"]!, "tests": records,
        ]
        if !natives.isEmpty {
            do {
                result["summary"] = try Self.profile(natives, destination: coverage, source: source)

            } catch { errors.append(String(describing: error)) }
        }
        let statements = (result["summary"] as? [String: Any])?["statements"] as? Int ?? 0
        result["status"] = !errors.isEmpty ? "incomplete" : statements > 0 ? "collected" : "no-data"
        result["errors"] = errors
        if !errors.isEmpty { try CoverageIO.remove(coverage.appendingPathComponent("index.html")) }
        try CoverageIO.write(records, to: coverage.appendingPathComponent("tests.json"))
        try CoverageIO.write(result, to: coverage.appendingPathComponent("summary.json"))
        let attempt = run.appendingPathComponent("attempt.json")
        if CoverageIO.exists(attempt) {
            var metadata = try CoverageIO.read(attempt)
            metadata["coverage"] = result.filter { $0.key != "tests" }
            if !errors.isEmpty && metadata["exitStatus"] as? Int == 0 { metadata["exitStatus"] = 1 }
            try CoverageIO.write(metadata, to: attempt)
        }
        for error in errors {
            FileHandle.standardError.write(Data("ERROR: E2E coverage: \(error)\n".utf8))
        }
        return errors.isEmpty ? 0 : 1
    }
}

struct GoCoverageBlock {
    let location: String
    let file: String
    let statements: Int
    let count: UInt64
    init(_ line: String) throws {
        let fields = line.split(separator: " ")
        guard fields.count == 3, let statements = Int(fields[1]), let count = UInt64(fields[2]),
            let colon = fields[0].lastIndex(of: ":")
        else { throw ValidationError("Malformed Go coverage block: " + line) }
        self.location = String(fields[0])
        self.file = String(fields[0][..<colon])
        self.statements = statements
        self.count = count
    }
}
