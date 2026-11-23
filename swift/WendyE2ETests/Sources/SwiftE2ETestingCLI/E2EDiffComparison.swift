import ArgumentParser
import Foundation
import WendyE2ETesting

/// The same validated comparison used by Go diff coverage and spec annotations.
struct E2EDiffComparison: Decodable {
    struct Provenance: Decodable {
        var baseRevision: String
        var sourceRevision: String
        var sourceID: String
        var patchSHA256: String
    }
    struct Source: Decodable {
        var path: String
        var sourceFilePath: String?
        var oldPath: String
        var baseSource: String?
        var source: String?
        var addedLines: [Int]
        var removedLines: [Int]
    }
    var status: String
    var reason: String?
    var provenance: Provenance?
    var files: [Source]
    private var baseDocuments: [WendyE2EReference.Document] = []

    private enum CodingKeys: String, CodingKey {
        case status, reason, provenance, files
    }

    init(at url: URL) throws {
        self = try JSONDecoder().decode(Self.self, from: Data(contentsOf: url))
        guard let provenance,
            provenance.baseRevision.count == 40, provenance.sourceRevision.count == 40,
            provenance.patchSHA256.count == 64
        else {
            throw ValidationError(
                "Spec comparison unavailable: \(reason ?? "missing revision provenance")"
            )
        }
        var originals: [String: String] = [:]
        for file in files {
            if let source = file.baseSource { originals[file.oldPath] = source }
        }
        baseDocuments = try originals.keys.sorted().flatMap { path in
            try WendyE2EReference.parseValidatedSource(originals[path]!, path: path)
        }
    }

    func annotate(
        _ documents: [WendyE2EReference.Document],
        at path: String,
        sourceFilePath: String? = nil
    ) throws
        -> [WendyE2EReference.Document]
    {
        guard
            let file = files.first(where: {
                path == $0.path || path.hasSuffix("/" + $0.path)
                    || (sourceFilePath != nil && sourceFilePath == $0.sourceFilePath)
            })
        else {
            return documents
        }
        guard let source = file.source,
            try String(contentsOfFile: path, encoding: .utf8) == source
        else {
            throw ValidationError(
                "Spec comparison source differs from the rendered source: \(path)"
            )
        }
        var marked = try WendyE2EReference.markingChanges(in: documents, comparedTo: baseDocuments)
        let original = baseDocuments.filter { $0.sourceLocation.path == file.oldPath }
        for d in marked.indices {
            for s in marked[d].sections.indices {
                for e in marked[d].sections[s].entries.indices {
                    var entry = marked[d].sections[s].entries[e]
                    let old = original.first { $0.title == marked[d].title }?
                        .sections.flatMap(\.entries).first { $0.title == entry.title }
                    let selected =
                        file.addedLines.contains {
                            (entry.declarationStartLine <= $0 && $0 <= entry.declarationEndLine)
                                || (old != nil
                                    && entry.inheritedSuiteTraitRange?.contains($0) == true)
                        }
                        || file.removedLines.contains { line in
                            old.map {
                                ($0.declarationStartLine <= line && line <= $0.declarationEndLine)
                                    || $0.inheritedSuiteTraitRange?.contains(line) == true
                            } ?? false
                        }
                    if !selected { entry.change = nil }
                    marked[d].sections[s].entries[e] = entry
                }
            }
        }
        return marked
    }
}

func e2eRunComparison(at runURL: URL, explicit: String?) throws -> E2EDiffComparison? {
    let attempts = try runAttemptArtifactURLsByTarget(in: runURL).values.flatMap { $0 }
    let url: URL
    if let explicit {
        url = URL(fileURLWithPath: explicit)
    } else {
        guard attempts.count == 1, let attempt = attempts.first else { return nil }
        url = attempt.appendingPathComponent("coverage/diff.json")
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        // Unavailable comparison data is shown by the total's label, without badges.
        let object = try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any]
        guard object?["provenance"] != nil else { return nil }
    }
    let comparison = try E2EDiffComparison(at: url)
    guard !attempts.isEmpty else {
        throw ValidationError("Spec comparison requires a matching run attempt")
    }
    for attempt in attempts {
        let info =
            try JSONSerialization.jsonObject(
                with: Data(contentsOf: attempt.appendingPathComponent("attempt.json"))
            ) as? [String: Any]
        guard let source = info?["git"] as? [String: Any],
            let commit = source["commit"] as? String,
            commit == comparison.provenance?.sourceRevision
        else {
            throw ValidationError("Spec comparison revision differs from the run attempt")
        }
    }
    return comparison
}
