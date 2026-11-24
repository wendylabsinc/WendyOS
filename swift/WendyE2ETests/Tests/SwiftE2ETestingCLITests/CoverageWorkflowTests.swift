import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct `coverage workflow evidence` {
    @Test
    func `failed aggregate or render still uploads the derived run and preserves failure status`()
        throws
    {
        let product = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent()
        let action = try String(
            contentsOf: product.appendingPathComponent(".github/actions/swift-e2e-run/action.yml"),
            encoding: .utf8
        )
        let block = action.components(
            separatedBy: "    - name: Render Swift E2E overview (Unix)\n"
        )[1].components(separatedBy: "\n    - name:")[0]
        let script = block.components(separatedBy: "      run: |\n")[1].components(
            separatedBy: "\n"
        ).map { String($0.dropFirst(min(8, $0.prefix(while: { $0 == " " }).count))) }.joined(
            separator: "\n"
        )
        for (aggregate, report) in [(0, 0), (7, 0), (0, 8), (7, 8)] {
            let root = try CoverageIO.temporaryDirectory()
            defer { try? FileManager.default.removeItem(at: root) }
            let scripts = root.appendingPathComponent("swift/Scripts")
            try FileManager.default.createDirectory(at: scripts, withIntermediateDirectories: true)
            try """
            #!/bin/bash
            mkdir -p "$2/swift.run/coverage"
            printf 'retained overall profile' > "$2/swift.run/coverage/coverage.out"
            exit "$AGGREGATE_EXIT"
            """.write(
                to: scripts.appendingPathComponent("E2EAggregate.sh"),
                atomically: true,
                encoding: .utf8
            )
            try "#!/bin/bash\nexit \"$REPORT_EXIT\"\n".write(
                to: scripts.appendingPathComponent("E2EReport.sh"),
                atomically: true,
                encoding: .utf8
            )
            let attempt = root.appendingPathComponent("wendy/test.run.local.0001")
            try FileManager.default.createDirectory(at: attempt, withIntermediateDirectories: true)
            var env = ProcessInfo.processInfo.environment
            env["RUNNER_TEMP"] = root.path
            env["RUN_ID"] = "test.run.local.0001"
            env["DIFF_BASE_REF"] = ""
            env["AGGREGATE_EXIT"] = String(aggregate)
            env["REPORT_EXIT"] = String(report)
            let result = try CoverageIO.execute(
                ["bash", "-euo", "pipefail", "-c", script],
                cwd: root,
                environment: env
            )
            #expect(result.status == (aggregate != 0 ? Int32(aggregate) : Int32(report)))
            #expect(
                try String(
                    contentsOf: attempt.appendingPathComponent("report/coverage/coverage.out"),
                    encoding: .utf8
                ) == "retained overall profile"
            )
        }
    }
}

@Suite
struct `aggregate wrapper arguments` {
    @Test func `forwards the explicit Git range unchanged to Swift`() throws {
        let product = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent()
        let root = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let tool = root.appendingPathComponent("bin/swift")
        let package = root.appendingPathComponent("package")
        let attempt = root.appendingPathComponent("swift.run.local.0001")
        for directory in [tool.deletingLastPathComponent(), package, attempt] {
            try FileManager.default.createDirectory(
                at: directory,
                withIntermediateDirectories: true
            )
        }
        try "#!/bin/bash\nprintf '%s\\0' \"$@\" > \"$ARGV_FILE\"\n".write(
            to: tool,
            atomically: true,
            encoding: .utf8
        )
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: tool.path)
        let arguments = root.appendingPathComponent("arguments")
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = tool.deletingLastPathComponent().path + ":" + env["PATH", default: ""]
        env["ARGV_FILE"] = arguments.path
        let result = try CoverageIO.execute(
            [
                "bash", product.appendingPathComponent("swift/Scripts/E2EAggregate.sh").path,
                "--package-dir", package.path, "--output-dir",
                root.appendingPathComponent("output").path, "--diff", "origin/main...HEAD",
                attempt.path,
            ],
            environment: env
        )
        #expect(result.status == 0)
        let values = try String(contentsOf: arguments, encoding: .utf8).components(
            separatedBy: "\0"
        )
        let index = try #require(values.firstIndex(of: "--diff"))
        #expect(values[index + 1] == "origin/main...HEAD")
        #expect(values.prefix(3) == ["run", "swift-e2e-testing", "aggregate"])
    }
}

@Suite
struct `native archive member paths` {
    @Test func `Mac file metadata cannot add unmanifested archive members`() throws {
        let root = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let native = root.appendingPathComponent("observations/suite/test/coverage/go")
        try FileManager.default.createDirectory(at: native, withIntermediateDirectories: true)
        let names = ["covmeta.fixture", "covcounters.fixture.1.0", "invocation.1.0"]
        for name in names {
            try Data("native bytes".utf8).write(to: native.appendingPathComponent(name))
        }
        #if os(macOS)
            _ = try CoverageIO.command([
                "xattr", "-w", "com.wendylabs.coverage-fixture", "file metadata",
                native.appendingPathComponent(names[0]).path,
            ])
        #endif
        try GoCoverage.pack(run: root)
        let archive = root.appendingPathComponent("coverage/go-native.tar.gz")
        let actual = Set(CoverageIO.lines(try CoverageIO.command(["tar", "-tzf", archive.path])))
        let expected = Set(names.map { "observations/suite/test/coverage/go/" + $0 })
        #expect(actual == expected)
        let recovered = root.appendingPathComponent("recovered")
        try FileManager.default.createDirectory(at: recovered, withIntermediateDirectories: true)
        _ = try CoverageIO.command(["tar", "-xzf", archive.path, "-C", recovered.path])
        for name in expected {
            #expect(
                try Data(contentsOf: recovered.appendingPathComponent(name))
                    == Data("native bytes".utf8)
            )
        }
    }
}
