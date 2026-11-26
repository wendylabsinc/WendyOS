import Foundation
import Testing
import WendyE2ETesting

let appsIsolationTestsEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ISOLATION"] == "true"

/// Isolated Agent protocol tier with synthetic registry/runtime peers.
/// This never establishes live Cloud, real DOM or containerd behavior.
enum AppsIsolationFixture {
    static func run(
        _ test: String,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        let allowed = [
            "TestAppsOperatorIsolationRemovalDuringPull",
            "TestAppsOperatorIsolationOverlappingStarts",
            "TestAppsOperatorIsolationBadDataRefused",
            "TestAppsOperatorIsolationResumeConfirmationRace",
        ]
        try #require(allowed.contains(test))
        let binary =
            test == "TestAppsOperatorIsolationResumeConfirmationRace"
            ? "apps-cli-isolation.test" : "apps-agent-isolation.test"
        let path = URL(fileURLWithPath: AppsDeploymentFixture.observerPath)
            .deletingLastPathComponent().appendingPathComponent(binary).path
        let package = URL(fileURLWithPath: AppsDeploymentFixture.observerPath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent(
                test == "TestAppsOperatorIsolationResumeConfirmationRace"
                    ? "go/internal/cli/commands" : "go/internal/agent/services"
            ).path
        // Acceptance always runs the complete existing aggregate. Focused Go
        // investigations are separate evidence and cannot satisfy this wrapper.
        let subcases =
            test == "TestAppsOperatorIsolationBadDataRefused"
            ? ["manifest", "blob", "wrap", "revision-zero", "wrong-target", "bad-signature"] : []
        guard FileManager.default.isExecutableFile(atPath: path) else {
            throw AppsDeploymentFixture.Prerequisite(
                description: "Compile the in-tree isolation test binaries before opting in"
            )
        }
        try await CLIAndAgentScenario().run(
            authenticated: false,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, _ in
            try await cli.sh(
                "cd \(AppsDeploymentFixture.quote(package)) && env WENDY_E2E_APPS_ISOLATION=true \(AppsDeploymentFixture.quote(path)) -test.v -test.count=1 -test.timeout=45s -test.run '^\(test)$'"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("--- PASS: \(test)"))
                #expect(!result.stdout.contains("--- SKIP: \(test)"))
                #expect(!result.stdout.contains("[no tests to run]"))
                #expect(!result.stdout.contains("[no test files]"))
                for subcase in subcases {
                    #expect(result.stdout.contains("--- PASS: \(test)/\(subcase)"))
                }
            }
        }
    }
}
