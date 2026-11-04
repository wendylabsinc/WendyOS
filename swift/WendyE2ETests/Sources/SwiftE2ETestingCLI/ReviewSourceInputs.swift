import ArgumentParser
import Foundation

struct ReviewSourceInputs {
    var patchURL: URL
    var manifestURL: URL
    var sourceFilesURL: URL
    var changedFilesURL: URL
    var diffstatURL: URL
    var policyRule: String
    var toolReadLimitation: String
    var status: String
}

/// Not a command-line mistake, so ArgumentParser must not follow it with usage text.
struct ReviewSourcePreparationError: Error, CustomStringConvertible {
    var description: String
}

func prepareReviewSourceInputs(
    repoURL: URL,
    runURL: URL,
    adapterURL: URL,
    policyRef: String,
    diff: String?
) throws -> ReviewSourceInputs {
    let process = Process()
    let errors = Pipe()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    process.arguments =
        [
            "python3", "-I", adapterURL.path, "--repo", repoURL.path,
            "--run-dir", runURL.path, "--policy-ref", policyRef,
        ] + (diff.map { ["--diff", $0] } ?? [])
    process.currentDirectoryURL = repoURL
    process.standardInput = FileHandle.nullDevice
    process.standardError = errors
    try process.run()
    // Drain before waiting: Python/Git diagnostics must not fill a pipe.
    let errorData = errors.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    guard process.terminationStatus == 0 else {
        throw ReviewSourcePreparationError(
            description: String(decoding: errorData, as: UTF8.self)
                .trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }
    struct Metadata: Decodable {
        var policy_rule: String
        var tool_read_limitation: String
        var status: String
    }
    let manifestURL = runURL.appendingPathComponent("git-diff-source-input.json")
    let metadata = try JSONDecoder().decode(Metadata.self, from: Data(contentsOf: manifestURL))
    return ReviewSourceInputs(
        patchURL: runURL.appendingPathComponent("git-diff-source.diff"),
        manifestURL: manifestURL,
        sourceFilesURL: runURL.appendingPathComponent("review-source-files.json"),
        changedFilesURL: runURL.appendingPathComponent("git-diff-name-only.txt"),
        diffstatURL: runURL.appendingPathComponent("git-diff-stat.txt"),
        policyRule: metadata.policy_rule,
        toolReadLimitation: metadata.tool_read_limitation,
        status: metadata.status
    )
}
