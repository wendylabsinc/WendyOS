import ArgumentParser
import Foundation
import WendyE2ETesting

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
        try WendyE2ECoverage.validateNative(natives)
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
        try WendyE2ECoverage.checkCLICounters(native)
    }

    static func agentControl(
        _ path: String,
        operation: String,
        directory: String = ""
    ) throws -> Int {
        try WendyE2ECoverage.agentControl(path, operation: operation, directory: directory)
    }

    func startCapture(control: String?) throws {
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
        source["capture"] = ["status": "running"]
        try CoverageIO.write(source, to: coverage.appendingPathComponent("source.json"))
        try CoverageIO.write([], to: coverage.appendingPathComponent("tests.json"))
    }

    func finishCapture(control: String?) throws -> Int32 {
        let source = try CoverageIO.read(coverage.appendingPathComponent("source.json"))
        try Self.validateSource(source)
        var records = try CoverageIO.records(coverage.appendingPathComponent("tests.json"))
        var errors: [String] = []
        if CoverageIO.exists(run.appendingPathComponent("timeout.json")) {
            errors.append("Test process timed out; coverage intervals may be incomplete")
        }
        let interrupted = CoverageIO.exists(coverage.appendingPathComponent("current-test.txt"))
        if let index = records.indices.last, records[index]["status"] as? String == "incomplete" {
            let receipt = coverage.appendingPathComponent("current-test.txt")
            if CoverageIO.exists(receipt) {
                do {
                    let test = URL(
                        fileURLWithPath: try String(contentsOf: receipt, encoding: .utf8)
                    ).resolvingSymlinksInPath()
                    records[index]["directory"] = try CoverageIO.relative(test, to: run)
                    if let control, records[index]["agentExported"] as? Bool != true {
                        let pid = try Self.agentControl(
                            control,
                            operation: "export",
                            directory: test.appendingPathComponent("coverage/go").path
                        )
                        guard pid == records[index]["agentPID"] as? Int else {
                            throw ValidationError("Agent changed during interrupted scenario")
                        }
                        records[index]["agentExported"] = true
                    }
                } catch { errors.append(String(describing: error)) }
            }
        }
        if source["capture"] != nil {
            var updatedSource = source
            do {
                guard !interrupted else {
                    throw ValidationError(
                        "Scenario interrupted; remaining suite tests may be unexecuted"
                    )
                }
                let xml = run.appendingPathComponent("test-results.xml")
                let data = try Data(contentsOf: xml)
                let parser = XMLParser(data: data)
                guard parser.parse(), parser.parserError == nil else {
                    throw ValidationError(
                        "Suite interrupted or produced invalid xUnit; remaining tests may be unexecuted"
                    )
                }
                let cases = try XMLDocument(data: data).nodes(forXPath: "//testcase")
                var seen = Set<Int>()
                for item in cases {
                    guard let test = item as? XMLElement,
                        let classname = test.attribute(forName: "classname")?.stringValue,
                        let name = test.attribute(forName: "name")?.stringValue
                    else { throw ValidationError("Suite xUnit case has no test identity") }
                    let identity =
                        normalizedClassname(classname) + "/" + normalizedTestName(name) + "()"
                    let skipped = try !test.nodes(forXPath: "skipped").isEmpty
                    let failed = try !test.nodes(forXPath: "failure|error").isEmpty
                    if let i = records.firstIndex(where: { $0["test"] as? String == identity }) {
                        seen.insert(i)
                        records[i]["testExitStatus"] = failed ? 1 : 0
                    } else {
                        records.append([
                            "test": identity, "sourceID": source["sourceID"]!,
                            "status": skipped ? "skipped" : "no-data",
                            "cli": "not invoked through the scenario harness",
                            "testExitStatus": failed ? 1 : 0,
                        ])
                        seen.insert(records.count - 1)
                    }
                }
                guard records.indices.allSatisfy({ seen.contains($0) }) else {
                    throw ValidationError(
                        "Scenario capture missing from suite xUnit; suite may have been interrupted"
                    )
                }
                let info = try CoverageIO.read(run.appendingPathComponent("attempt.json"))
                let failures = try cases.reduce(0) {
                    try $0 + ($1.nodes(forXPath: "failure|error").isEmpty ? 0 : 1)
                }
                if (info["exitStatus"] as? Int ?? 0) != 0 && failures == 0 {
                    throw ValidationError(
                        "Suite exited unsuccessfully without a recorded test failure; remaining tests may be unexecuted"
                    )
                }
                updatedSource["capture"] = ["status": "completed", "cases": cases.count]
            } catch {
                let detail = "Suite capture incomplete: \(error)"
                errors.append(detail)
                updatedSource["capture"] = [
                    "status": "incomplete", "error": detail,
                    "unexecuted": "Unknown after suite interruption; no outcomes inferred",
                ]
            }
            try CoverageIO.write(updatedSource, to: coverage.appendingPathComponent("source.json"))
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
        if let capture = source["capture"] as? [String: Any],
            capture["status"] as? String != "completed"
        {
            errors.append(
                capture["error"] as? String
                    ?? "Suite capture incomplete; remaining tests may be unexecuted"
            )
        }
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
            guard let directory = record["directory"] as? String else {
                let error = "Missing collected test directory: \(record["test"] ?? "")"
                errors.append(error)
                record["status"] = "incomplete"
                record["error"] = error
                record["nativeValidated"] = false
                records[index] = record
                continue
            }
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
        // tests.json is capture evidence; derived statuses belong in the summary.
        try CoverageIO.write(result, to: coverage.appendingPathComponent("summary.json"))
        let attempt = run.appendingPathComponent("attempt.json")
        if CoverageIO.exists(attempt) {
            var metadata = try CoverageIO.read(attempt)
            metadata["coverage"] = result.filter { $0.key != "tests" }
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
