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
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: deploy")
        func testSingleFlagDeploy() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: no-restart")
        func testSingleFlagNoRestart() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.noRestart = true

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-unless-stopped")
        func testSingleFlagRestartUnlessStopped() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.restartUnlessStoppedFlag = true

            // Should not throw
            try cmd.validate()
        }

        @Test("Allow single flag: restart-on-failure")
        func testSingleFlagRestartOnFailure() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.restartOnFailureRetries = 10

            // Should not throw
            try cmd.validate()
        }

        @Test("Reject conflicting flags: deploy + no-restart")
        func testConflictingDeployAndNoRestart() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true
            cmd.noRestart = true

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-on-failure")
        func testConflictingDeployAndRestartOnFailure() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true
            cmd.restartOnFailureRetries = 10

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: deploy + restart-unless-stopped")
        func testConflictingDeployAndRestartUnlessStopped() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true
            cmd.restartUnlessStoppedFlag = true

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-unless-stopped")
        func testConflictingNoRestartAndRestartUnlessStopped() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.noRestart = true
            cmd.restartUnlessStoppedFlag = true

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: no-restart + restart-on-failure")
        func testConflictingNoRestartAndRestartOnFailure() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.noRestart = true
            cmd.restartOnFailureRetries = 5

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject conflicting flags: restart-unless-stopped + restart-on-failure")
        func testConflictingRestartUnlessStoppedAndRestartOnFailure() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.restartUnlessStoppedFlag = true
            cmd.restartOnFailureRetries = 3

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }

        @Test("Reject three conflicting flags")
        func testThreeConflictingFlags() throws {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true
            cmd.noRestart = true
            cmd.restartUnlessStoppedFlag = true

            #expect(throws: (any Error).self) {
                try cmd.validate()
            }
        }
    }

    // MARK: - isDetached Property Tests

    @Suite("isDetached Computed Property")
    struct IsDetachedTests {

        @Test("isDetached returns false by default")
        func testIsDetachedDefault() {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()

            #expect(cmd.isDetached == false)
        }

        @Test("isDetached returns true when deploy is set")
        func testIsDetachedWithDeploy() {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when detach is set")
        func testIsDetachedWithDetach() {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.detach = true

            #expect(cmd.isDetached == true)
        }

        @Test("isDetached returns true when both deploy and detach are set")
        func testIsDetachedWithBoth() {
            var cmd = RunCommand()
            cmd.agentConnectionOptions = AgentConnectionOptions()
            cmd.deploy = true
            cmd.detach = true

            #expect(cmd.isDetached == true)
        }
    }
}
