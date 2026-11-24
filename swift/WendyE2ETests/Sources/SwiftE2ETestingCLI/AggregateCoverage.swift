import ArgumentParser
import Foundation

/// Native files are only unpacked into scratch space. Reaggregation can change
/// the comparison base without executing tests or expanding delivered evidence.
struct AggregateCoverageAttempt {
    let input: URL
    let scratch: URL
    let source: [String: Any]
    let result: [String: Any]
    let natives: [URL]

    init(input: URL, repo: URL) throws {
        self.input = input
        let workspace = try CoverageIO.temporaryDirectory()
        scratch = workspace
        do {
            for name in [
                "coverage/source.json", "coverage/tests.json", "attempt.json", "timeout.json",
            ] {
                let original = input.appendingPathComponent(name)
                if CoverageIO.exists(original) {
                    let destination = workspace.appendingPathComponent(name)
                    try FileManager.default.createDirectory(
                        at: destination.deletingLastPathComponent(),
                        withIntermediateDirectories: true
                    )
                    try FileManager.default.copyItem(at: original, to: destination)
                }
            }
            let archive = input.appendingPathComponent("coverage/go-native.tar.gz")
            if CoverageIO.exists(archive) {
                let members = CoverageIO.lines(
                    try CoverageIO.command(["tar", "-tzf", archive.path])
                )
                guard
                    members.allSatisfy({ name in
                        let parts = name.split(separator: "/")
                        return !name.hasPrefix("/") && !parts.contains("..")
                            && ((parts.count == 6 && parts[0] == "observations"
                                && parts[3] == "coverage" && parts[4] == "go")
                                || (parts.count == 3 && parts[0] == "coverage"
                                    && parts[1] == "bootstrap"))
                    })
                else { throw ValidationError("Unsafe native archive member") }
                _ = try CoverageIO.command(["tar", "-xzf", archive.path, "-C", workspace.path])
                let manifest = try String(
                    contentsOf: input.appendingPathComponent("coverage/native.sha256"),
                    encoding: .utf8
                )
                let entries = try CoverageIO.lines(manifest).map { line -> (String, String) in
                    guard line.count > 66 else {
                        throw ValidationError("Invalid native SHA manifest")
                    }
                    return (String(line.prefix(64)), String(line.dropFirst(66)))
                }
                guard Set(entries.map(\.1)) == Set(members) else {
                    throw ValidationError("Native archive differs from manifest")
                }
                for entry in entries {
                    let attributes = try FileManager.default.attributesOfItem(
                        atPath: workspace.appendingPathComponent(entry.1).path
                    )
                    guard attributes[.type] as? FileAttributeType == .typeRegular else {
                        throw ValidationError("Non-file native archive entry")
                    }
                }
                guard
                    try CoverageIO.hashes(entries.map { workspace.appendingPathComponent($0.1) })
                        == entries.map(\.0)
                else { throw ValidationError("Native archive hash mismatch") }
            } else {
                for original in try GoCoverage.nativeFiles(run: input) {
                    let destination = workspace.appendingPathComponent(
                        try CoverageIO.relative(original, to: input)
                    )
                    try FileManager.default.createDirectory(
                        at: destination.deletingLastPathComponent(),
                        withIntermediateDirectories: true
                    )
                    try FileManager.default.copyItem(at: original, to: destination)
                }
            }
            let records = try CoverageIO.records(
                workspace.appendingPathComponent("coverage/tests.json")
            )
            for record in records {
                if let directory = record["directory"] as? String {
                    let original = try CoverageIO.evidence(directory + "/test.json", in: input)
                    if CoverageIO.exists(original) {
                        let destination = try CoverageIO.evidence(
                            directory + "/test.json",
                            in: workspace
                        )
                        try FileManager.default.createDirectory(
                            at: destination.deletingLastPathComponent(),
                            withIntermediateDirectories: true
                        )
                        try FileManager.default.copyItem(at: original, to: destination)
                    }
                }
            }
            let provenance = try CoverageIO.read(
                workspace.appendingPathComponent("coverage/source.json")
            )
            try GoCoverage.validateSource(provenance)
            let info = try CoverageIO.read(workspace.appendingPathComponent("attempt.json"))
            guard
                (info["git"] as? [String: Any])?["commit"] as? String == provenance["revision"]
                    as? String
            else { throw ValidationError("Attempt revision differs from coverage build") }
            _ = try GoCoverage(repo: repo, run: workspace).deriveAttempt()
            source = provenance
            result = try CoverageIO.read(workspace.appendingPathComponent("coverage/summary.json"))
            let derived = result["tests"] as! [[String: Any]]
            var natives: [URL] = []
            for record in derived {
                guard let directory = record["directory"] as? String else { continue }
                let metadata = try CoverageIO.evidence(directory + "/test.json", in: workspace)
                if CoverageIO.exists(metadata) {
                    let info = try CoverageIO.read(metadata)
                    if record["nativeValidated"] as? Bool == true {
                        natives.append(
                            try CoverageIO.evidence(directory + "/coverage/go", in: workspace)
                        )
                    }
                    try CoverageIO.write(
                        info,
                        to: try CoverageIO.evidence(directory + "/test.json", in: input)
                    )
                }
            }
            self.natives = natives
            for name in ["coverage/summary.json", "attempt.json"] {
                try CoverageIO.write(
                    try JSONSerialization.jsonObject(
                        with: Data(contentsOf: workspace.appendingPathComponent(name))
                    ),
                    to: input.appendingPathComponent(name)
                )
            }
        } catch {
            try? FileManager.default.removeItem(at: workspace)
            throw error
        }
    }
}

func aggregateGoCoverage(
    inputs: [URL],
    captured: [AggregateCoverageAttempt],
    errors initialErrors: [String],
    run: URL,
    repo: URL?,
    diff: String?
) throws -> Bool {
    let destination = run.appendingPathComponent("coverage")
    try CoverageIO.remove(run.appendingPathComponent("comparison.json"))
    // Never reuse a profile/totals left by another set of attempts.
    try CoverageIO.remove(destination)
    var errors = initialErrors
    var result: [String: Any] = ["status": "not-collected", "errors": errors]
    let first = captured.first
    var compatible = true
    if let first {
        let identity = first.source["sourceID"] as? String
        let revision = first.source["revision"] as? String
        func scope(_ source: [String: Any]) throws -> Data {
            try JSONSerialization.data(
                withJSONObject: [
                    "files": source["files"]!, "packages": source["packages"]!,
                    "counterMode": source["counterMode"]!,
                ],
                options: [.sortedKeys]
            )
        }
        let expectedScope = try scope(first.source)
        for attempt in captured {
            if try attempt.source["sourceID"] as? String != identity
                || attempt.source["revision"] as? String != revision
                || scope(attempt.source) != expectedScope
            {
                compatible = false
                errors.append(
                    "Incompatible coverage source revision or build scope: "
                        + attempt.input.lastPathComponent
                )
            }
            errors += attempt.result["errors"] as? [String] ?? []
        }
        if captured.count != inputs.count {
            errors.append("Some run attempts have no compatible captured coverage")
        }
        result["sourceID"] = identity
        result["sourceRevision"] = revision
        result["scope"] = first.source["scope"]
        let natives = captured.flatMap(\.natives)
        if compatible && !natives.isEmpty {
            do {
                if captured.count == 1, let summary = first.result["summary"] {
                    try FileManager.default.createDirectory(
                        at: destination,
                        withIntermediateDirectories: true
                    )
                    try FileManager.default.copyItem(
                        at: first.scratch.appendingPathComponent("coverage/coverage.out"),
                        to: destination.appendingPathComponent("coverage.out")
                    )
                    result["summary"] = summary
                } else {
                    result["summary"] = try GoCoverage.profile(
                        natives,
                        destination: destination,
                        source: first.source
                    )
                }
            } catch { errors.append(String(describing: error)) }
        }
        let statements = (result["summary"] as? [String: Any])?["statements"] as? Int ?? 0
        result["status"] =
            errors.isEmpty ? (statements == 0 ? "no-data" : "collected") : "incomplete"
    } else if !errors.isEmpty {
        result["status"] = "incomplete"
    }
    result["attempts"] = captured.map {
        [
            "attempt": $0.input.lastPathComponent, "sourceID": $0.source["sourceID"]!,
            "revision": $0.source["revision"]!, "status": $0.result["status"]!,
        ]
    }
    result["errors"] = errors
    if let diff {
        var comparison = E2ERunComparison(status: "unavailable", range: diff)
        do {
            guard let repo else {
                throw ValidationError("Comparison needs the matching source repository")
            }
            let revisions = try Set(
                inputs.compactMap { input in
                    (try CoverageIO.read(input.appendingPathComponent("attempt.json"))["git"]
                        as? [String: Any])?["commit"] as? String
                }
            )
            guard revisions.count == 1, let revision = revisions.first else {
                throw ValidationError("Comparison needs one immutable tested revision")
            }
            let git = try E2EGitComparison(repo: repo, range: diff, testedRevision: revision)
            comparison.status = "compared"
            comparison.baseRevision = git.base
            comparison.sourceRevision = git.head
            comparison.specs = try git.specs()
            if let first {
                guard compatible, first.source["revision"] as? String == git.head else {
                    throw ValidationError("Comparison source differs from measured build")
                }
                comparison.sourceID = first.source["sourceID"] as? String
                let profile = destination.appendingPathComponent("coverage.out")
                if CoverageIO.exists(profile) {
                    comparison.profileSHA256 = try CoverageIO.sha256(file: profile)
                }
                var changed = try git.goCoverage(
                    source: first.source,
                    profile: profile,
                    complete: errors.isEmpty && CoverageIO.exists(profile)
                )
                changed["provenance"] = [
                    "baseRevision": git.base, "sourceRevision": git.head,
                    "sourceID": comparison.sourceID ?? "",
                    "profileSHA256": comparison.profileSHA256.map { $0 as Any } ?? NSNull(),
                ]
                result["diff"] = changed
            } else {
                result["diff"] = [
                    "status": "unavailable", "reason": "Go coverage not captured",
                    "percent": NSNull(),
                ]
            }
        } catch {
            comparison.status = "unavailable"
            comparison.reason = String(describing: error)
            comparison.specs = []
            result["diff"] = [
                "status": "unavailable", "reason": String(describing: error), "percent": NSNull(),
            ]
        }
        if let detail = result["diff"] as? [String: Any],
            detail["status"] as? String == "unavailable"
        {
            let reason = detail["reason"] as? String ?? "No comparison result"
            FileHandle.standardError.write(
                Data("WARNING: E2E aggregate comparison unavailable: \(reason)\n".utf8)
            )
        }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        try encoder.encode(comparison).write(
            to: run.appendingPathComponent("comparison.json"),
            options: .atomic
        )
    }
    if !captured.isEmpty || !errors.isEmpty || diff != nil {
        try CoverageIO.write(result, to: destination.appendingPathComponent("summary.json"))
    }
    for error in errors {
        FileHandle.standardError.write(Data("ERROR: E2E aggregate coverage: \(error)\n".utf8))
    }
    return !errors.isEmpty
}
