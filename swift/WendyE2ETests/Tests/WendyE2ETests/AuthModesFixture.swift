import Foundation
import Testing
import WendyE2ETesting

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

let authModesUnprivilegedFileTestsSupported: Bool = {
    #if canImport(Darwin) || canImport(Glibc) || canImport(Musl)
        return geteuid() != 0
    #else
        return false
    #endif
}()

let authModesLocalTestsSupported = WendyE2EMachine.cli.isLocal && WendyE2EMachine.cli.os != .windows

/// Runs in-tree isolated protocol/admission assertions through the normal
/// scenario and recorder. These observations never establish board behavior.
enum AuthModesFixture {
    static let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().path

    static func quote(_ text: String) -> String {
        "'" + text.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    struct ReopenDuringRestart: Error {}

    struct GoEvent: Decodable {
        let Action: String
        let Test: String?
        let Output: String?
    }

    static func isolated(
        _ package: String,
        _ tests: [String],
        knownReopen: Bool = false,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try #require(!tests.isEmpty)
        try #require(WendyE2EMachine.cli.isLocal && WendyE2EMachine.cli.os != .windows)
        try #require(
            WendyE2EEnvironment.isolation != .none,
            "Auth Modes requires isolated CLI state"
        )
        let cache = root + "/Build/auth-modes-go-cache"
        let originalHome = try #require(ProcessInfo.processInfo.environment["HOME"])
        let moduleCache =
            ProcessInfo.processInfo.environment["GOMODCACHE"] ?? originalHome + "/go/pkg/mod"
        try await CLIAndAgentScenario().run(
            authenticated: false,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, _ in
            let result = try await cli.sh(
                """
                set -eu
                cd \(quote(root + "/go"))
                env GOCACHE=\(quote(cache)) GOMODCACHE=\(quote(moduleCache)) \
                  WENDY_AUTH_MODES_CLI="$(command -v wendy)" \
                  go test -json -count=1 -timeout=120s \(quote(package)) \
                  -run \(quote("^(" + tests.joined(separator: "|") + ")$"))
                """
            ) { $0 }
            let events = try result.stdout.split(separator: "\n").map {
                try JSONDecoder().decode(GoEvent.self, from: Data($0.utf8))
            }
            // Match only the reproduced functional assertion. Compilation, setup,
            // timeouts, missing tests and all other failures remain ordinary failures.
            let regression = "TestAuthModesA23ReopenDuringRestart"
            let expectedFailure =
                knownReopen && tests == [regression]
                && events.contains { $0.Action == "fail" && $0.Test == regression }
                && events.compactMap(\.Output).joined().contains(
                    "was Closed when this computer last reached it"
                )
                && events.compactMap(\.Output).joined().contains(
                    "CLI: exit status 5; want \"Access: Open\""
                )
            if expectedFailure
                && ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_STRICT"] != "true"
            {
                try withKnownIssue(
                    "WDY-4393: polling during restart recreates the cleared Closed pin",
                    {
                        throw ReopenDuringRestart()
                    },
                    matching: { $0.error is ReopenDuringRestart }
                )
                return
            }
            #expect(result.status.isSuccess)
            // A missing, skipped, failed or vacuous Go selection fails this spec.
            #expect(!events.contains { $0.Action == "skip" || $0.Action == "fail" })
            for test in tests {
                #expect(
                    events.contains { $0.Action == "pass" && $0.Test == test },
                    "Missing completed assertion group: \(test)"
                )
            }
        }
    }
}
