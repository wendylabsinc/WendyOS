package import Foundation

/// Scenario-owned counter interval. Derivation and packaging remain in aggregate.
package struct WendyE2ECoverage: Sendable {
    private let run: URL
    private let directory: String
    private let control: String?
    private var coverage: URL { run.appendingPathComponent("coverage") }

    package static func begin(
        recorder: WendyE2ERecorder,
        environment: [String: String] = ProcessInfo.processInfo.environment
    ) throws -> Self? {
        let enabled = ["1", "true", "yes", "on", "enabled"]
        guard enabled.contains(environment["WENDY_E2E_COVERAGE"]?.lowercased() ?? "") else {
            return nil
        }
        guard !enabled.contains(environment["WENDY_E2E_PARALLEL"]?.lowercased() ?? ""),
            let root = environment["WENDY_E2E_RUN_DIR"]
        else {
            throw failure("Coverage requires sequential scenario execution and a run directory")
        }
        let run = URL(fileURLWithPath: root).resolvingSymlinksInPath()
        let test = URL(fileURLWithPath: recorder.testDirectoryPath).resolvingSymlinksInPath()
        guard test.path.hasPrefix(run.path + "/observations/") else {
            throw failure("Coverage recorder is outside the attempt")
        }
        let capture = Self(
            run: run,
            directory: String(test.path.dropFirst(run.path.count + 1)),
            control: environment["WENDY_E2E_COVERAGE_CONTROL"].flatMap { $0.isEmpty ? nil : $0 }
        )
        let source = try read(capture.coverage.appendingPathComponent("source.json"))
        guard let identity = source["sourceID"] as? String else {
            throw failure("Missing coverage build provenance")
        }
        // Exclusive creation refuses overlapping/nested scopes even if the caller
        // forgot --no-parallel. The durable receipt also survives a suite crash.
        let receipt = capture.coverage.appendingPathComponent("current-test.txt")
        try Data(test.path.utf8).write(to: receipt, options: .withoutOverwriting)
        do {
            try FileManager.default.createDirectory(
                at: test.appendingPathComponent("coverage/go"),
                withIntermediateDirectories: true
            )
            try recorder.writeMetadata()
            var records = try capture.records()
            guard !records.contains(where: { $0["directory"] as? String == capture.directory })
            else { throw failure("Coverage supports one scenario interval per test") }
            var record: [String: Any] = [
                "test": recorder.testIdentifier, "directory": capture.directory,
                "sourceID": identity, "status": "incomplete",
            ]
            records.append(record)
            try write(records, to: capture.coverage.appendingPathComponent("tests.json"))
            do {
                if (source["packages"] as? [String: Any])?["wendy-agent"] != nil {
                    guard let control = capture.control else {
                        throw failure(
                            "Instrumented managed Agent requires its private coverage control"
                        )
                    }
                    record["agentPID"] = try agentControl(control, operation: "reset")
                    records[records.count - 1] = record
                    try write(records, to: capture.coverage.appendingPathComponent("tests.json"))
                }
            } catch {
                record["error"] = String(describing: error)
                records[records.count - 1] = record
                try write(records, to: capture.coverage.appendingPathComponent("tests.json"))
                throw error
            }
            return capture
        } catch {
            try? FileManager.default.removeItem(at: receipt)
            throw error
        }
    }

    /// Called after all test-owned cleanup, also when scenario setup/body throws.
    package func finish() throws {
        var records = try records()
        guard let index = records.firstIndex(where: { $0["directory"] as? String == directory })
        else {
            throw Self.failure("Missing active scenario capture record")
        }
        let native = run.appendingPathComponent(directory + "/coverage/go")
        var record = records[index]
        defer {
            try? FileManager.default.removeItem(
                at: coverage.appendingPathComponent("current-test.txt")
            )
        }
        do {
            if let control {
                guard
                    try Self.agentControl(control, operation: "export", directory: native.path)
                        == record["agentPID"] as? Int
                else {
                    throw Self.failure("Agent changed between reset and export")
                }
                record["agentExported"] = true
            }
            let count = try Self.checkCLICounters(native)
            record["cliInvocations"] = count
            record["cli"] = count > 0 ? "collected" : "not invoked"
            if count > 0 || control != nil {
                try Self.validateNative([native])
                record["status"] = "collected"
            } else {
                record["status"] = "no-data"
            }
            records[index] = record
            try Self.write(records, to: coverage.appendingPathComponent("tests.json"))
        } catch {
            record["status"] = "incomplete"
            record["error"] = String(describing: error)
            records[index] = record
            try Self.write(records, to: coverage.appendingPathComponent("tests.json"))
            throw error
        }
    }

    private func records() throws -> [[String: Any]] {
        guard
            let records = try JSONSerialization.jsonObject(
                with: Data(contentsOf: coverage.appendingPathComponent("tests.json"))
            ) as? [[String: Any]],
            records.allSatisfy({
                $0["test"] is String && $0["status"] is String && $0["sourceID"] is String
            })
        else { throw Self.failure("Invalid capture journal") }
        return records
    }
    private static func read(_ url: URL) throws -> [String: Any] {
        guard
            let value = try JSONSerialization.jsonObject(with: Data(contentsOf: url))
                as? [String: Any]
        else { throw failure("Invalid capture provenance") }
        return value
    }
    private static func write(_ value: Any, to url: URL) throws {
        try JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys])
            .write(to: url, options: .atomic)
    }
    static func failure(_ message: String) -> NSError {
        NSError(
            domain: "WendyE2E.Coverage",
            code: 1,
            userInfo: [NSLocalizedDescriptionKey: message]
        )
    }
    static func files(_ root: URL, prefix: String) throws -> [URL] {
        guard FileManager.default.fileExists(atPath: root.path) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: root,
            includingPropertiesForKeys: nil
        )
        .filter { $0.lastPathComponent.hasPrefix(prefix) }
    }
}

extension WendyE2ECoverage {
    package static func validateNative(_ natives: [URL]) throws {
        for native in natives {
            let metadata = Set(
                try files(native, prefix: "covmeta.").map {
                    String($0.lastPathComponent.dropFirst(8))
                }
            )
            let counterFiles = try files(native, prefix: "covcounters.")
            let counters = try Set(
                counterFiles.map { path -> String in
                    let parts = path.lastPathComponent.components(separatedBy: ".")
                    guard parts.count == 4, !parts[1].isEmpty, parts[1].allSatisfy(\.isHexDigit),
                        let pid = Int(parts[2]), pid >= 0, let timestamp = Int64(parts[3]),
                        timestamp >= 0
                    else {
                        throw failure(
                            "Malformed native Go counter filename: " + path.lastPathComponent
                        )
                    }
                    return parts[1]
                }
            )
            guard !metadata.isEmpty && !counters.isEmpty else {
                throw failure("Missing native Go metadata/counters: " + native.path)
            }
            guard counters.isSubset(of: metadata) else {
                throw failure("Missing native Go metadata for counters: " + native.path)
            }
            guard metadata.isSubset(of: counters) else {
                throw failure("Missing native Go counters for metadata: " + native.path)
            }
        }
    }

    package static func checkCLICounters(_ native: URL) throws -> Int {
        let counters = try files(native, prefix: "covcounters.")
        let starts = try files(native, prefix: "invocation.")
        for start in starts {
            let fields = start.lastPathComponent.components(separatedBy: ".")
            guard fields.count == 3, let timestamp = Int64(fields[2]),
                counters.contains(where: {
                    let parts = $0.lastPathComponent.components(separatedBy: ".")
                    return parts.count == 4 && parts[2] == fields[1]
                        && (Int64(parts[3]) ?? -1) >= timestamp
                })
            else {
                throw failure(
                    "CLI invocation did not write counters: " + start.lastPathComponent
                )
            }
        }
        if try !starts.isEmpty
            && (counters.isEmpty || files(native, prefix: "covmeta.").isEmpty)
        {
            throw failure("Missing native Go counters or metadata")
        }
        return starts.count
    }

}
