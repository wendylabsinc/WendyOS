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
                try RunCommand.parse(["--restart-unless-stopped", "--restart-on-failure", "3"]).validate()
            }
        }

        @Test("Reject three conflicting flags")
        func testThreeConflictingFlags() throws {
            #expect(throws: (any Error).self) {
                let cmd = try RunCommand.parse(["--deploy", "--no-restart", "--restart-unless-stopped"])
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
}
