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
            var cmd = try RunCommand.parseAsRoot([]) as! RunCommand

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: deploy")
        func testSingleFlagDeploy() throws {
            var cmd = try RunCommand.parseAsRoot(["--deploy"]) as! RunCommand

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: no-restart")
        func testSingleFlagNoRestart() throws {
            var cmd = try RunCommand.parseAsRoot(["--no-restart"]) as! RunCommand

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-unless-stopped")
        func testSingleFlagRestartUnlessStopped() throws {
            var cmd = try RunCommand.parseAsRoot(["--restart-unless-stopped"]) as! RunCommand

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-on-failure")
        func testSingleFlagRestartOnFailure() throws {
            var cmd = try RunCommand.parseAsRoot(["--restart-on-failure", "10"]) as! RunCommand

            // Should not throw
            try cmd.validate()
        }

        @Test("Reject conflicting flags: deploy + no-restart")
        func testConflictingDeployAndNoRestart() throws {
            var cmd = try RunCommand.parseAsRoot(["--deploy", "--no-restart"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-on-failure")
        func testConflictingDeployAndRestartOnFailure() throws {
            var cmd = try RunCommand.parseAsRoot(["--deploy", "--restart-on-failure", "10"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-unless-stopped")
        func testConflictingDeployAndRestartUnlessStopped() throws {
            var cmd = try RunCommand.parseAsRoot(["--deploy", "--restart-unless-stopped"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-unless-stopped")
        func testConflictingNoRestartAndRestartUnlessStopped() throws {
            var cmd = try RunCommand.parseAsRoot(["--no-restart", "--restart-unless-stopped"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-on-failure")
        func testConflictingNoRestartAndRestartOnFailure() throws {
            var cmd = try RunCommand.parseAsRoot(["--no-restart", "--restart-on-failure", "5"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: restart-unless-stopped + restart-on-failure")
        func testConflictingRestartUnlessStoppedAndRestartOnFailure() throws {
            var cmd = try RunCommand.parseAsRoot(["--restart-unless-stopped", "--restart-on-failure", "3"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject three conflicting flags")
        func testThreeConflictingFlags() throws {
            var cmd = try RunCommand.parseAsRoot(["--deploy", "--no-restart", "--restart-unless-stopped"]) as! RunCommand

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }
    }

    // MARK: - isDetached Property Tests

    @Suite("isDetached Computed Property")
    struct IsDetachedTests {

        @Test("isDetached returns false by default")
        func testIsDetachedDefault() throws {
            let cmd = try RunCommand.parseAsRoot([]) as! RunCommand

            #expect(cmd.isDetached == false)
        }

        @Test("isDetached returns true when deploy is set")
        func testIsDetachedWithDeploy() throws {
            let cmd = try RunCommand.parseAsRoot(["--deploy"]) as! RunCommand

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when detach is set")
        func testIsDetachedWithDetach() throws {
            let cmd = try RunCommand.parseAsRoot(["--detach"]) as! RunCommand

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when both deploy and detach are set")
        func testIsDetachedWithBoth() throws {
            let cmd = try RunCommand.parseAsRoot(["--deploy", "--detach"]) as! RunCommand

            #expect(cmd.isDetached == true)
        }
    }
}
