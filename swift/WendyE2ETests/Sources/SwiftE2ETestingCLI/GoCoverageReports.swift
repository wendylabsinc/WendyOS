import ArgumentParser
import Foundation

/// The numbers come from aggregate. Source views are optional and require the same checkout.
struct GoCoverageReports {
    enum Stage { case aggregate, report }
    let run: URL
    let root: URL
    let attempts: [String: [URL]]
    private var linked = Set<URL>()

    init(run: URL, package: URL, repo: URL?, stage: Stage = .report) throws {
        self.run = run
        root = run.appendingPathComponent("coverage")
        attempts = try runAttemptArtifactURLsByTarget(in: run)
        let all = attempts.values.flatMap { $0 }
        var directories: [(URL, [URL])] = [(root, all)]
        if attempts.count > 1 {
            for (target, members) in attempts {
                directories.append((try directory(for: target), members))
            }
        }
        for attempt in all {
            directories.append((attempt.appendingPathComponent("coverage"), [attempt]))
        }
        var sources: [String: GoReportSource] = [:]
        for (directory, members) in directories {
            let profile = directory.appendingPathComponent("coverage.out")
            let html = directory.appendingPathComponent("source.html")
            let functions = directory.appendingPathComponent("functions.txt")
            // Validate the existing parent before creating an output, including on macOS
            // where resolving a nonexistent child can retain a /private prefix.
            guard CoverageIO.exists(directory) else { continue }
            _ = try CoverageIO.relative(directory, to: run)
            for url in [profile, html, functions, directory.appendingPathComponent("summary.json")]
            where CoverageIO.exists(url) {
                _ = try CoverageIO.relative(url, to: run)
            }
            guard CoverageIO.exists(profile) else {
                try CoverageIO.remove(html)
                try CoverageIO.remove(functions)
                continue
            }
            do {
                let summary = try CoverageIO.read(directory.appendingPathComponent("summary.json"))
                guard let identity = summary["sourceID"] as? String,
                    summary["status"] as? String == "collected"
                else {
                    try CoverageIO.remove(html)
                    try CoverageIO.remove(functions)
                    continue
                }
                guard let totals = summary["summary"] as? [String: Any],
                    let statements = totals["statements"] as? Int,
                    let covered = totals["covered"] as? Int
                else { throw ValidationError("Missing measured statement totals") }
                // The function list is an aggregate artifact and remains usable without a source checkout.
                if stage == .report, CoverageIO.exists(functions) { linked.insert(functions) }
                if sources[identity] == nil {
                    guard
                        let manifest = members.map({
                            $0.appendingPathComponent("coverage/source.json")
                        })
                        .first(where: CoverageIO.exists)
                    else { throw ValidationError("Missing Go coverage source manifest") }
                    _ = try CoverageIO.relative(manifest, to: run)
                    let repository: URL
                    if let repo {
                        repository = repo
                    } else {
                        let path = try CoverageIO.command(
                            ["git", "rev-parse", "--show-toplevel"],
                            cwd: package
                        )
                        repository = URL(
                            fileURLWithPath: path.trimmingCharacters(in: .whitespacesAndNewlines)
                        )
                    }
                    sources[identity] = try GoReportSource(
                        manifest: manifest,
                        repo: repository,
                        identity: identity
                    )
                }
                guard let source = sources[identity] else { continue }
                if let revision = summary["sourceRevision"] as? String, revision != source.revision
                {
                    throw ValidationError(
                        "Coverage summary revision differs from its source manifest"
                    )
                }
                // Older aggregates have only a profile. Backfill their function list on first render.
                var outputs: [(String, URL)] = []
                if stage == .aggregate || !CoverageIO.exists(functions) {
                    outputs.append(("-func", functions))
                }
                if stage == .report { outputs.append(("-html", html)) }
                try source.render(
                    profile: profile,
                    statements: statements,
                    covered: covered,
                    outputs: outputs
                )
                linked.insert(functions)
                if stage == .report { linked.insert(html) }
            } catch {
                try CoverageIO.remove(html)
                if stage == .aggregate { try CoverageIO.remove(functions) }
                FileHandle.standardError.write(
                    Data(
                        "WARNING: Go coverage source views unavailable for \(directory.path): \(error)\n"
                            .utf8
                    )
                )
            }
        }
    }

    func directory(for target: String) throws -> URL {
        if attempts.count == 1, attempts[target] != nil { return root }
        return try CoverageIO.evidence("targets/" + target, in: root)
    }

    func values(for target: String, directory: URL, comparing: Bool) throws -> String {
        let summaryURL = directory.appendingPathComponent("summary.json")
        let total: ReportCoverageValue
        let diff: ReportCoverageValue
        if CoverageIO.exists(summaryURL) {
            let summary = try CoverageIO.read(summaryURL)
            total = e2eGoCoverageTotal(summary)
            diff = e2eGoDiffCoverageTotal(
                summary["diff"] as? [String: Any],
                revision: summary["sourceRevision"] as? String
            )
        } else if let members = attempts[target], members.count == 1 {
            total = e2eAttemptGoCoverageTotal(at: members[0])
            diff = e2eAttemptGoDiffCoverageTotal(at: members[0])
        } else {
            total = .status("Not collected")
            diff = .status("Not compared")
        }
        return coverageValues(total: total, diff: comparing ? diff : nil)
    }

    func links(directory: URL, icons: Bool) -> String {
        let document =
            #"<svg aria-hidden="true" width="15" height="16" viewBox="0 0 18 20" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M3 1.5h8l4 4v13H3z M11 1.5v4h4 M7.5 9l-2 2 2 2 M10.5 9l2 2-2 2"/></svg>"#
        let function = #"<span class="function-glyph" aria-hidden="true">ƒ(x)</span>"#
        let links = [
            ("source.html", "HTML", "source coverage report", document),
            ("functions.txt", "functions", "function coverage list", function),
        ].map { file, text, label, icon in
            guard linked.contains(directory.appendingPathComponent(file)),
                let relative = try? CoverageIO.relative(
                    directory.appendingPathComponent(file),
                    to: run
                )
            else {
                if !icons { return "" }
                return
                    "<span class=\"observation-button\" aria-disabled=\"true\" role=\"img\" aria-label=\"No \(label) available\" title=\"No \(label) available\">\(icon)</span>"
            }
            let url = relative.split(separator: "/").map {
                String($0).addingPercentEncoding(
                    withAllowedCharacters: .alphanumerics.union(CharacterSet(charactersIn: "-._~"))
                ) ?? ""
            }.joined(separator: "/")
            let style = icons ? " class=\"observation-button\"" : ""
            return
                "<a\(style) href=\"\(escapeHTML(url))\" aria-label=\"Open \(label)\" title=\"Open \(label)\">\(icons ? icon : text)</a>"
        }.filter { !$0.isEmpty }.joined(separator: icons ? "" : ", ")
        if icons { return "<span class=\"target-actions\">\(links)</span>" }
        return links.isEmpty ? "Coverage" : "Coverage: " + links
    }
}

func coverageValues(total: ReportCoverageValue, diff: ReportCoverageValue?) -> String {
    func value(_ label: String, _ coverage: ReportCoverageValue) -> String {
        "<span title=\"\(escapeHTML(coverage.detail))\">\(label) <b>\(coverage.visible)</b></span>"
    }
    return (diff.map { value("Diff", $0) + " · " } ?? "") + value("Total", total)
}

/// Resolve profile filenames against the measured module without asking Go to fetch packages.
private struct GoReportSource {
    let repo: URL
    let revision: String
    let paths: [String: String]

    init(manifest: URL, repo: URL, identity: String) throws {
        self.repo = repo
        let source = try CoverageIO.read(manifest)
        try GoCoverage.validateSource(source)
        revision = try CoverageIO.command(["git", "rev-parse", "HEAD"], cwd: repo)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard let files = source["files"] as? [String: [String: Any]],
            source["sourceID"] as? String == identity, source["revision"] as? String == revision
        else { throw ValidationError("Checkout revision differs from the measured Go source") }
        var paths: [String: String] = [:]
        for (name, record) in files {
            guard let path = record["path"] as? String, path.hasSuffix(".go"),
                !path.contains("\\"), !path.contains("\n"), !path.contains("\r"),
                name.hasSuffix("/" + path),
                try CoverageIO.relative(CoverageIO.evidence(path, in: repo), to: repo) == path
            else { throw ValidationError("Invalid measured Go source path") }
            if record["generated"] as? Bool == false, record["testHarness"] as? Bool == false {
                paths[name] = path
            }
        }
        guard try CoverageIO.sourceID(repo: repo, paths: GoCoverage.sourcePaths(files)) == identity
        else {
            throw ValidationError("Checkout contents differ from the measured Go source")
        }
        self.paths = paths
    }

    func render(profile: URL, statements: Int, covered: Int, outputs: [(String, URL)]) throws {
        let scratch = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: scratch) }
        let lines = CoverageIO.lines(try String(contentsOf: profile, encoding: .utf8))
        guard lines.first == "mode: atomic" else {
            throw ValidationError("Expected an atomic Go profile")
        }
        var local = "mode: atomic\n"
        var count = 0
        var hits = 0
        for line in lines.dropFirst() {
            let block = try GoCoverageBlock(line)
            guard let relative = paths[block.file] else {
                throw ValidationError("Unknown profile source: " + block.file)
            }
            guard block.statements >= 0, block.statements <= Int.max - count else {
                throw ValidationError("Invalid statement count in Go profile")
            }
            count += block.statements
            if block.count > 0 { hits += block.statements }
            local += "./" + relative + line.dropFirst(block.file.count) + "\n"
        }
        guard count == statements, hits == covered else {
            throw ValidationError("Go profile differs from saved statement totals")
        }
        let input = scratch.appendingPathComponent("coverage.out")
        try local.write(to: input, atomically: true, encoding: .utf8)
        // Generate into scratch first, so a failed tool cannot publish a partial page or list.
        for (option, output) in outputs {
            let temporary = scratch.appendingPathComponent(output.lastPathComponent)
            _ = try CoverageIO.command(
                ["go", "tool", "cover", option + "=" + input.path, "-o", temporary.path],
                cwd: repo
            )
            try Data(contentsOf: temporary).write(to: output, options: .atomic)
        }
    }
}
