import Foundation

#if canImport(FoundationXML)
    import FoundationXML
#endif

let e2eAttemptArtifactsDirectoryName = "attempts"
let e2eObservationsDirectoryName = "observations"
let e2eSourceArtifactFileName = "source.md"
let e2eSourceIndexFileName = "source-index.md"

private let e2eAttemptJSONMaxBytes = 1_048_576
private let e2eAttemptXUnitMaxBytes = 50 * 1_048_576
private let e2eAttemptMessageMaxCharacters = 2_048

func e2eAttemptArtifactsRootURL(in runURL: URL) -> URL {
    runURL.appendingPathComponent(e2eAttemptArtifactsDirectoryName, isDirectory: true)
}

func e2eObservationsRootURL(in runURL: URL) -> URL {
    runURL.appendingPathComponent(e2eObservationsDirectoryName, isDirectory: true)
}

func e2eAttemptArtifactsURL(in runURL: URL, targetName: String, attempt: String) -> URL {
    e2eAttemptArtifactsRootURL(in: runURL)
        .appendingPathComponent(targetName, isDirectory: true)
        .appendingPathComponent(attempt, isDirectory: true)
}

func e2eAttemptExitStatus(at attemptURL: URL) -> Int? {
    guard let object = e2eAttemptJSON(at: attemptURL) else { return nil }
    return object["exitStatus"] as? Int
}

func e2eAttemptXUnitURL(at attemptURL: URL) -> URL? {
    let normalizedURL = attemptURL.appendingPathComponent("test-results.xml")
    if FileManager.default.fileExists(atPath: normalizedURL.path) {
        return normalizedURL
    }

    let swiftTestingURL = attemptURL.appendingPathComponent("test-results-swift-testing.xml")
    if FileManager.default.fileExists(atPath: swiftTestingURL.path) {
        return swiftTestingURL
    }

    return nil
}

func e2eAttemptInfrastructureFailureDetail(at attemptURL: URL) -> String? {
    if let timeout = e2eAttemptTimeoutDetail(at: attemptURL) {
        return timeout
    }

    let xunitProblem = e2eAttemptXUnitProblem(at: attemptURL)
    if let exitStatus = e2eAttemptExitStatus(at: attemptURL), exitStatus != 0 {
        if let xunitProblem {
            return
                "attempt exited with status \(exitStatus) and produced unusable xUnit output: \(xunitProblem)"
        }
        if e2eAttemptXUnitURL(at: attemptURL) == nil {
            return "attempt exited with status \(exitStatus) before producing test-results.xml"
        }
    }

    if let xunitProblem {
        return "attempt produced unusable xUnit output: \(xunitProblem)"
    }

    return nil
}

private func e2eAttemptJSON(at attemptURL: URL) -> [String: Any]? {
    let url = attemptURL.appendingPathComponent("attempt.json")
    guard FileManager.default.fileExists(atPath: url.path),
        let data = e2eReadDataIfSmall(at: url, maxBytes: e2eAttemptJSONMaxBytes),
        let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
    else {
        return nil
    }
    return object
}

private func e2eAttemptTimeoutDetail(at attemptURL: URL) -> String? {
    let url = attemptURL.appendingPathComponent("timeout.json")
    guard FileManager.default.fileExists(atPath: url.path) else { return nil }
    if let size = e2eFileSize(url), size > e2eAttemptJSONMaxBytes {
        return "timeout.json is too large to inspect (\(size) bytes)"
    }
    if let data = e2eReadDataIfSmall(at: url, maxBytes: e2eAttemptJSONMaxBytes),
        let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
        let message = object["message"] as? String,
        !message.isEmpty
    {
        return e2eBoundedMessage(message)
    }
    return "Swift E2E attempt timed out"
}

private func e2eBoundedMessage(_ message: String) -> String {
    let normalized =
        message
        .replacingOccurrences(of: "\r", with: " ")
        .replacingOccurrences(of: "\n", with: " ")
    guard normalized.count > e2eAttemptMessageMaxCharacters else { return normalized }
    return String(normalized.prefix(e2eAttemptMessageMaxCharacters)) + "…"
}

private func e2eAttemptXUnitProblem(at attemptURL: URL) -> String? {
    guard let url = e2eAttemptXUnitURL(at: attemptURL) else { return nil }
    if let size = e2eFileSize(url), size > e2eAttemptXUnitMaxBytes {
        return "\(url.lastPathComponent) is too large to validate (\(size) bytes)"
    }
    guard let data = e2eReadDataIfSmall(at: url, maxBytes: e2eAttemptXUnitMaxBytes) else {
        return "could not read \(url.lastPathComponent)"
    }
    guard !data.isEmpty else {
        return "\(url.lastPathComponent) is empty"
    }

    let parser = XMLParser(data: data)
    // Linux Foundation's parse() returns true for truncated documents and only
    // reports the premature end through parserError.
    if parser.parse(), parser.parserError == nil {
        return nil
    }

    return "\(url.lastPathComponent) is truncated or not parseable"
}

private func e2eReadDataIfSmall(at url: URL, maxBytes: Int) -> Data? {
    guard let size = e2eFileSize(url), size <= maxBytes else { return nil }
    return try? Data(contentsOf: url)
}

private func e2eFileSize(_ url: URL) -> Int? {
    (try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? nil
}
