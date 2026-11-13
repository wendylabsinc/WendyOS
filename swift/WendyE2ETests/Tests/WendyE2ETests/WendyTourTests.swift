import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy tour'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays simulator-first and physical-device routes for `wendy tour`. The
     output includes the command synopsis, local flags, inherited global flags,
     and concise descriptions. Help exits successfully, writes to stdout, emits
     no stderr, and leaves configuration, cache, project, cloud, and device state
     untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy tour --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Watch a simulated Go2 walk"))
                #expect(result.stdout.contains("wendy tour [flags]"))
                #expect(result.stdout.contains("--simulator"))
                #expect(result.stdout.contains("--hardware"))
                #expect(result.stdout.contains("--project"))
                #expect(result.stdout.contains("--help"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Presents the new-user tour as an interactive sequence covering auth,
     project setup, device discovery, and deployment choices. Each step
     explains what happens before performing side effects.
     */
    @Test(
        .disabled(
            "WDY-1951: the multi-step Bubble Tea tour needs scripted PTY screen synchronization plus isolated auth, project, discovery, install, MCP, and deployment fixtures."
        )
    )
    func `runs the guided setup tour interactively`() async throws {
        // TODO: enable with the scripted isolated tour fixture (WDY-1951).
    }

    /**
     Existing auth sessions, projects, or configured devices are detected
     and presented as completed rather than repeated unnecessarily.
     */
    @Test(
        .disabled(
            "WDY-1951: completed-step coverage needs a scripted PTY and isolated fixture state for every downstream tour integration."
        )
    )
    func `skips completed steps using existing state`() async throws {
        // TODO: enable with the scripted isolated tour fixture (WDY-1951).
    }

    /**
     Cancelling the tour stops at the current step, preserves state already
     confirmed by the user, and avoids starting later steps.
     */
    @Test(
        .disabled(
            "WDY-1951: cancellation checkpoints need PTY process control and observable isolated side effects to prove later steps did not run."
        )
    )
    func `cancels without continuing later side effects`() async throws {
        // TODO: enable with scripted cancellation control (WDY-1951).
    }

    /**
     Without an explicit route in a non-interactive terminal, exits instead of
     blocking for input and directs the user to the simulator or interactive
     hardware setup.
     */
    @Test
    func `runs safely in non-interactive contexts`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy tour") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")

                let envelope = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stderr.utf8))
                        as? [String: Any]
                )
                let error = try #require(envelope["error"] as? [String: Any])
                #expect(error["code"] as? String == "error")
                #expect(error["exit"] as? Int == 1)
                #expect(
                    error["message"] as? String
                        == "choose 'wendy tour --simulator' or run 'wendy tour --hardware' in an interactive terminal"
                )
                #expect(error["retryable"] as? Bool == false)
                #expect(error["next_steps"] as? [String] == [])
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy tour --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Rejects positional arguments because this command is entirely flag-driven.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy tour' silently accepts extra positional arguments because the command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when tour rejects positional arguments (WDY-1934).
    }
}
