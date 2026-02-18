import ArgumentParser
import Foundation
import Testing

@testable import wendy

@Suite("RunCommand Tests")
struct RunCommandTests {

    // MARK: - Flag Validation Tests

    @Suite("Flag Validation")
    struct FlagValidationTests {

        @Test("Allow no flags - default development mode")
        func testNoFlags() throws {
            // Parse with no restart policy flags
            let cmd = try RunCommand.parse([])

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: deploy")
        func testSingleFlagDeploy() throws {
            let cmd = try RunCommand.parse(["--deploy"])

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: no-restart")
        func testSingleFlagNoRestart() throws {
            let cmd = try RunCommand.parse(["--no-restart"])

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-unless-stopped")
        func testSingleFlagRestartUnlessStopped() throws {
            let cmd = try RunCommand.parse(["--restart-unless-stopped"])

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-on-failure")
        func testSingleFlagRestartOnFailure() throws {
            let cmd = try RunCommand.parse(["--restart-on-failure", "10"])

            // Should not throw
            try cmd.validate()
        }

        @Test("Reject conflicting flags: deploy + no-restart")
        func testConflictingDeployAndNoRestart() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--deploy", "--no-restart"]).validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-on-failure")
        func testConflictingDeployAndRestartOnFailure() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--deploy", "--restart-on-failure", "10"]).validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-unless-stopped")
        func testConflictingDeployAndRestartUnlessStopped() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--deploy", "--restart-unless-stopped"]).validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-unless-stopped")
        func testConflictingNoRestartAndRestartUnlessStopped() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--no-restart", "--restart-unless-stopped"]).validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-on-failure")
        func testConflictingNoRestartAndRestartOnFailure() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--no-restart", "--restart-on-failure", "5"]).validate()
            }
        }

        @Test("Reject conflicting flags: restart-unless-stopped + restart-on-failure")
        func testConflictingRestartUnlessStoppedAndRestartOnFailure() throws {
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--restart-unless-stopped", "--restart-on-failure", "3"])
                    .validate()
            }
        }

        @Test("Reject three conflicting flags")
        func testThreeConflictingFlags() throws {
            #expect(throws: (any Error).self) {
                let cmd = try RunCommand.parse([
                    "--deploy", "--no-restart", "--restart-unless-stopped",
                ])
                try cmd.validate()
            }
        }
    }

    // MARK: - isDetached Property Tests

    @Suite("isDetached Computed Property")
    struct IsDetachedTests {

        @Test("isDetached returns false by default")
        func testIsDetachedDefault() throws {
            let cmd = try RunCommand.parse([])

            #expect(cmd.isDetached == false)
        }

        @Test("isDetached returns true when deploy is set")
        func testIsDetachedWithDeploy() throws {
            let cmd = try RunCommand.parse(["--deploy"])

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when detach is set")
        func testIsDetachedWithDetach() throws {
            let cmd = try RunCommand.parse(["--detach"])

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when both deploy and detach are set")
        func testIsDetachedWithBoth() throws {
            let cmd = try RunCommand.parse(["--deploy", "--detach"])

            #expect(cmd.isDetached == true)
        }
    }

    // MARK: - Restart Policy Tests

    @Suite("Restart Policy Building")
    struct RestartPolicyTests {

        @Test("Default mode builds 'no' restart policy")
        func testDefaultRestartPolicy() throws {
            let cmd = try RunCommand.parse([])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .no)
            #expect(policy.onFailureMaxRetries == 0)
        }

        @Test("Deploy mode builds 'on-failure' with 5 retries")
        func testDeployRestartPolicy() throws {
            let cmd = try RunCommand.parse(["--deploy"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .onFailure)
            #expect(policy.onFailureMaxRetries == 5)
        }

        @Test("No-restart flag builds 'no' restart policy")
        func testNoRestartPolicy() throws {
            let cmd = try RunCommand.parse(["--no-restart"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .no)
            #expect(policy.onFailureMaxRetries == 0)
        }

        @Test("Restart-unless-stopped flag builds 'unless-stopped' policy")
        func testRestartUnlessStoppedPolicy() throws {
            let cmd = try RunCommand.parse(["--restart-unless-stopped"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .unlessStopped)
        }

        @Test("Restart-on-failure with custom retries builds correct policy")
        func testRestartOnFailureWithCustomRetries() throws {
            let cmd = try RunCommand.parse(["--restart-on-failure", "3"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .onFailure)
            #expect(policy.onFailureMaxRetries == 3)
        }

        @Test("Restart-on-failure with 10 retries")
        func testRestartOnFailureWith10Retries() throws {
            let cmd = try RunCommand.parse(["--restart-on-failure", "10"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .onFailure)
            #expect(policy.onFailureMaxRetries == 10)
        }

        @Test("Restart-on-failure with 1 retry")
        func testRestartOnFailureWith1Retry() throws {
            let cmd = try RunCommand.parse(["--restart-on-failure", "1"])
            let policy = cmd.buildRestartPolicy()

            #expect(policy.mode == .onFailure)
            #expect(policy.onFailureMaxRetries == 1)
        }

        @Test("Priority: no-restart takes precedence (tested via validation)")
        func testNoRestartTakesPrecedence() throws {
            // This is validated by flag validation tests
            // If multiple flags are set, validate() throws
            // This test documents that priority is enforced by validation
            #expect(throws: (any Error).self) {
                try RunCommand.parse(["--no-restart", "--deploy"]).validate()
            }
        }
    }

    @Suite("Shell Helpers")
    struct ShellHelpersTests {
        @Test("shellInvocationArguments uses fish-compatible flags")
        func shellInvocationArgumentsUsesFishFlags() {
            let args = RunCommand.shellInvocationArguments(
                shell: "/opt/homebrew/bin/fish",
                command: "echo hi"
            )
            #expect(args == ["-c", "echo hi"])
        }

        @Test("shellInvocationArguments uses platform-specific shell variants")
        func shellInvocationArgumentsSupportsKnownShells() {
            #expect(
                RunCommand.shellInvocationArguments(shell: "pwsh", command: "echo hi") == [
                    "-Command", "echo hi",
                ]
            )
            #expect(
                RunCommand.shellInvocationArguments(shell: "cmd.exe", command: "echo hi") == [
                    "/C", "echo hi",
                ]
            )
            #expect(
                RunCommand.shellInvocationArguments(shell: "/bin/zsh", command: "echo hi") == [
                    "-lc", "echo hi",
                ]
            )
        }

        @Test("shellEscape handles empty input and quoting safely")
        func shellEscapeHandlesEmptyAndQuotedValues() {
            #if os(Windows)
                #expect(RunCommand.shellEscape("", shell: "cmd.exe") == "\"\"")
                #expect(
                    RunCommand.shellEscape("A&B", shell: "cmd.exe") == "\"A^&B\""
                )
                #expect(
                    RunCommand.shellEscape("it's", shell: "pwsh") == "'it''s'"
                )
            #else
                #expect(RunCommand.shellEscape("", shell: "/bin/zsh") == "''")
                #expect(RunCommand.shellEscape("hello world", shell: "/bin/zsh") == "'hello world'")
                #expect(RunCommand.shellEscape("it's", shell: "/bin/zsh") == "'it'\\''s'")
            #endif
        }

        @Test("sanitizeTemplateDeviceHost strips unsafe characters")
        func sanitizeTemplateDeviceHostStripsUnsafeCharacters() {
            #expect(
                RunCommand.sanitizeTemplateDeviceHost("jetson.local; rm -rf /")
                    == "jetson.local-rm-rf"
            )
            #expect(RunCommand.sanitizeTemplateDeviceHost("    ") == "")
            #expect(RunCommand.sanitizeTemplateDeviceHost("!!!") == "unknown-device")
        }
    }
}
