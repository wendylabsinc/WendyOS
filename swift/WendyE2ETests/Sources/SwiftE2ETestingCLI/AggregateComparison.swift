import ArgumentParser
import Foundation
import WendyE2ETesting

/// Aggregate saves results, not a patch or an alternate rendering-time diff.
struct E2ERunComparison: Codable {
    struct Spec: Codable {
        var path: String
        var line: Int
        var suite: String
        var name: String
        var change: String
        var sourceSHA256: String
    }
    var status: String
    var reason: String?
    var range: String
    var baseRevision: String?
    var sourceRevision: String?
    var sourceID: String?
    var profileSHA256: String?
    var specs: [Spec] = []

    static func artifact(in run: URL) -> URL {
        let current = run.appendingPathComponent("diff.json")
        return CoverageIO.exists(current) ? current : run.appendingPathComponent("comparison.json")
    }

    static func load(in run: URL) throws -> Self? {
        let path = artifact(in: run)
        guard CoverageIO.exists(path) else { return nil }
        let result = try JSONDecoder().decode(Self.self, from: Data(contentsOf: path))
        if result.status == "compared" {
            let attempts = try runAttemptArtifactURLsByTarget(in: run).values.flatMap { $0 }
            guard !attempts.isEmpty,
                try attempts.allSatisfy({
                    (try CoverageIO.read($0.appendingPathComponent("attempt.json"))["git"]
                        as? [String: Any])?["commit"] as? String == result.sourceRevision
                })
            else { throw ValidationError("Saved comparison revision differs from the run") }
            if let identity = result.sourceID {
                guard
                    try attempts.allSatisfy({
                        (try CoverageIO.read($0.appendingPathComponent("attempt.json"))["coverage"]
                            as? [String: Any])?["sourceID"] as? String == identity
                    })
                else { throw ValidationError("Saved comparison source differs from the run") }
            }
            if let hash = result.profileSHA256 {
                guard
                    try CoverageIO.sha256(file: run.appendingPathComponent("coverage/coverage.out"))
                        == hash
                else { throw ValidationError("Saved comparison profile differs from the run") }
            }
        }
        return result
    }
}

struct E2EGitComparison {
    let repo: URL
    let base: String
    let head: String
    let range: String
    let renamedPaths: [String: String]

    init(repo: URL, range: String, testedRevision: String) throws {
        self.repo = repo
        let range = range.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !range.isEmpty && !range.hasPrefix("-") else {
            throw ValidationError("--diff requires a Git revision range")
        }
        self.range = range
        func revision(_ value: String) throws -> String {
            guard !value.isEmpty && !value.hasPrefix("-") else {
                throw ValidationError("Invalid comparison revision")
            }
            return try CoverageIO.command(
                ["git", "rev-parse", "--verify", value + "^{commit}"],
                cwd: repo
            ).trimmingCharacters(in: .whitespacesAndNewlines)
        }
        let separator = range.contains("...") ? "..." : ".."
        let parts = range.components(separatedBy: separator)
        guard parts.count == 2 else {
            throw ValidationError("Use --diff BASE...HEAD or BASE..HEAD")
        }
        let left = try revision(parts[0])
        head = try revision(parts[1])
        guard head == testedRevision else {
            throw ValidationError("Diff head differs from the tested source revision")
        }
        base =
            separator == "..."
            ? try CoverageIO.command(["git", "merge-base", left, head], cwd: repo)
                .trimmingCharacters(in: .whitespacesAndNewlines) : left
        let names = try CoverageIO.command(
            ["git", "diff", "--name-status", "-z", "--find-renames", base, head],
            cwd: repo
        ).components(separatedBy: "\0")
        var renamed: [String: String] = [:]
        var index = 0
        while index + 1 < names.count && !names[index].isEmpty {
            let status = names[index]
            let old = names[index + 1]
            index += 2
            if status.hasPrefix("R") || status.hasPrefix("C") {
                guard index < names.count else {
                    throw ValidationError("Invalid Git rename output")
                }
                renamed[names[index]] = old
                index += 1
            }
        }
        renamedPaths = renamed
    }

    func blob(_ revision: String, path: String) throws -> String? {
        let result = try CoverageIO.execute(["git", "show", revision + ":" + path], cwd: repo)
        return result.status == 0 ? result.text : nil
    }

    /// Ordinary zero-context Git hunks. No token/AST interpretation: comments
    /// and formatting within an intersecting block intentionally can count.
    func changedLines(path: String) throws -> Set<Int> {
        var paths = [path]
        if let old = renamedPaths[path] { paths.append(old) }
        let patch = try CoverageIO.command(
            ["git", "diff", "--no-ext-diff", "--no-color", "--unified=0", base, head, "--"] + paths,
            cwd: repo
        )
        let expression = try NSRegularExpression(
            pattern: #"(?m)^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@"#
        )
        var result = Set<Int>()
        for match in expression.matches(in: patch, range: NSRange(patch.startIndex..., in: patch)) {
            let start = Int((patch as NSString).substring(with: match.range(at: 1)))!
            let count =
                match.range(at: 2).location == NSNotFound
                ? 1 : Int((patch as NSString).substring(with: match.range(at: 2)))!
            if count > 0 { result.formUnion(start..<(start + count)) }
        }
        return result
    }

    func changedPaths() throws -> [String] {
        try CoverageIO.command(
            ["git", "diff", "--name-only", "-z", "--no-renames", base, head],
            cwd: repo
        )
        .components(separatedBy: "\0").filter { !$0.isEmpty }
    }

    func goCoverage(source: [String: Any], profile: URL, complete: Bool) throws -> [String: Any] {
        let dirty = try CoverageIO.command(
            [
                "git", "diff", head, "--", "*.go", "go.mod", "go.sum", ".gitattributes",
                ":(glob)**/.gitattributes",
            ],
            cwd: repo
        )
        let untracked = try CoverageIO.command(
            ["git", "ls-files", "--others", "--exclude-standard", "--", "*.go"],
            cwd: repo
        )
        guard dirty.isEmpty && untracked.isEmpty else {
            throw ValidationError(
                "Comparison needs the recorded committed Go source and exclusions"
            )
        }
        let files = source["files"] as! [String: Any]
        var changed: [String: Set<Int>] = [:]
        let known = Set(GoCoverage.sourcePaths(files))
        for path in try changedPaths()
        where path.hasPrefix("go/") && path.hasSuffix(".go") && !path.hasSuffix("_test.go")
            && !path.split(separator: "/").contains("testdata")
            && !path.hasPrefix("go/internal/testfixture/")
            && !GoCoverage.hookPaths.contains(path)
        {
            guard let text = try blob(head, path: path), try !GoCoverage.generated(text) else {
                continue
            }
            let attribute = try CoverageIO.command(
                ["git", "check-attr", "--source=" + head, "linguist-generated", "--", path],
                cwd: repo
            ).trimmingCharacters(in: .whitespacesAndNewlines)
            if attribute.hasSuffix(": true") || attribute.hasSuffix(": set") { continue }
            if files.values.contains(where: { value in
                guard let file = value as? [String: Any] else { return false }
                return file["path"] as? String == path
                    && (file["generated"] as? Bool == true || file["testHarness"] as? Bool == true)
            }) {
                continue
            }
            let lines = try changedLines(path: path)
            if !lines.isEmpty { changed[path] = lines }
        }
        var blocks: [String: GoCoverageBlock] = [:]
        if CoverageIO.exists(profile) {
            for line in CoverageIO.lines(try String(contentsOf: profile, encoding: .utf8))
                .dropFirst()
            {
                let block = try GoCoverageBlock(line)
                guard let file = files[block.file] as? [String: Any],
                    let path = file["path"] as? String
                else { throw ValidationError("Profile outside recorded source scope") }
                guard let selected = changed[path], block.statements > 0 else { continue }
                let coordinates = block.location.components(separatedBy: ":").last!.components(
                    separatedBy: ","
                )
                let first = Int(coordinates[0].components(separatedBy: ".")[0])!
                let end = coordinates[1].components(separatedBy: ".")
                let last = Int(end[0])! - (end[1] == "1" ? 1 : 0)
                if selected.contains(where: { first <= $0 && $0 <= last }) {
                    blocks[block.location] = block
                }
            }
        }
        let missing = Set(changed.keys).subtracting(known).sorted()
        let statements = blocks.values.reduce(0) { $0 + $1.statements }
        let covered = blocks.values.reduce(0) { $0 + ($1.count > 0 ? $1.statements : 0) }
        let status =
            !missing.isEmpty || (!complete && !changed.isEmpty)
            ? "incomplete" : statements == 0 ? "not-applicable" : "collected"
        return [
            "status": status, "statements": statements, "covered": covered, "blocks": blocks.count,
            "missingFiles": missing,
            "percent": status == "collected"
                ? 100.0 * Double(covered) / Double(statements) as Any : NSNull(),
            "granularity":
                "Statements in Go blocks intersecting added/changed new-side line ranges; each block once; comments/formatting may count",
        ]
    }

    func specs() throws -> [E2ERunComparison.Spec] {
        let prefix = "swift/WendyE2ETests/Tests/WendyE2ETests/"
        let paths = try changedPaths().filter { $0.hasPrefix(prefix) && $0.hasSuffix(".swift") }
        struct Declaration {
            let path: String
            let document: WendyE2EReference.Document
            let entry: WendyE2EReference.Entry
            let hash: String
            var identity: String { document.title + "\0" + entry.title }
        }
        func declarations(_ revision: String) throws -> [String: Declaration] {
            var result: [String: Declaration] = [:]
            for path in paths {
                guard let source = try blob(revision, path: path) else { continue }
                let documents = try WendyE2EReference.parseValidatedSource(source, path: path)
                let hash = try CoverageIO.sha256(Data(source.utf8))
                for document in documents {
                    for entry in document.sections.flatMap(\.entries) {
                        let value = Declaration(
                            path: path,
                            document: document,
                            entry: entry,
                            hash: hash
                        )
                        guard result.updateValue(value, forKey: value.identity) == nil else {
                            throw ValidationError("Ambiguous spec identity: " + value.identity)
                        }
                    }
                }
            }
            return result
        }
        let before = try declarations(base)
        let after = try declarations(head)
        var result: [E2ERunComparison.Spec] = []
        for identity in Set(before.keys).union(after.keys).sorted() {
            let change: String
            let declaration: Declaration
            if let current = after[identity] {
                declaration = current
                if let old = before[identity] {
                    guard
                        old.entry.declarationSource != current.entry.declarationSource
                            || old.entry.availability != current.entry.availability
                            || old.entry.availabilityReason != current.entry.availabilityReason
                    else { continue }
                    change = "Changed"
                } else {
                    change = "Added"
                }
            } else {
                declaration = before[identity]!
                change = "Removed"
            }
            result.append(
                .init(
                    path: String(declaration.path.dropFirst("swift/WendyE2ETests/".count)),
                    line: declaration.entry.sourceLocation.line,
                    suite: declaration.document.title,
                    name: declaration.entry.title,
                    change: change,
                    sourceSHA256: declaration.hash
                )
            )
        }
        return result
    }
}
