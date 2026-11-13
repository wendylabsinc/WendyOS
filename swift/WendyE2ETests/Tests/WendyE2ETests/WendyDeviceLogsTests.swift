import Testing
import WendyE2ETesting

@Suite
struct `'wendy device logs'` {
    let scenario = CLIAndAgentScenario()

    /**
     A detached app writes a bounded 2 MiB to each of stdout and stderr before
     answering HTTP. With no log reader attached, both writes finish and the
     same process continues answering later requests. This guards against
     undrained output freezing an otherwise healthy app; it does not exercise
     crash-loop event attribution or an unlimited sustained output workload.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `keeps serving after output beyond pipe capacity without a log reader`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                let before = try await app.response(cli)
                let burst = try await app.response(
                    cli,
                    endpoint: "http://\(app.device):\(app.port)/burst",
                    retryStartup: false,
                    timeoutSeconds: 20
                )
                #expect(burst.burstBytesPerStream == 2 * 1024 * 1024)
                #expect(burst.instance == before.instance)
                #expect(burst.requests > before.requests)
                let after = try await app.response(cli, retryStartup: false)
                #expect(after.instance == before.instance)
                #expect(after.requests > burst.requests)
            }
        }
    }

    /**
     `--app --tail --no-follow --json` returns the selected app's real stdout
     and stderr as JSONL, including a Compose service member. Another app's
     later output does not displace this app's matching tail. An error-level
     replay excludes its INFO stdout while retaining its ERROR stderr.
     Reading logs leaves both services serving requests in the same processes.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `replays owned stdout and stderr with app and severity filters`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                let before = try await app.response(cli)
                _ = try await app.requireReplay(cli, containing: before)
                try await HTTPAppFixture.withComposeApp(cli: cli) { other in
                    let otherBefore = try await other.response(cli)
                    _ = try await other.requireReplay(cli, containing: otherBefore)
                    // Each subsequent write occurs after the preceding output
                    // is observed in stored replay. These three distinct
                    // ingestion boundaries exceed the target's two-batch tail;
                    // a global-tail-before-filter implementation loses it.
                    for _ in 0..<2 {
                        let noise = try await other.response(cli, retryStartup: false)
                        _ = try await other.requireReplay(cli, containing: noise)
                    }
                    let entries = try await app.logs(cli, tail: 2)
                    try #require(!entries.isEmpty, "Replay must contain actual app output")
                    #expect(
                        entries.contains {
                            $0.body.contains(app.marker("stdout", before))
                                && $0.attributes?["stream"] == "stdout"
                        }
                    )
                    #expect(
                        entries.contains {
                            $0.body.contains(app.marker("stderr", before))
                                && $0.attributes?["stream"] == "stderr"
                        }
                    )
                    #expect(entries.allSatisfy { !$0.body.contains(other.app) })
                    #expect(entries.allSatisfy { !$0.timestamp.isEmpty && !$0.severity.isEmpty })

                    let errors = try await app.logs(cli, tail: 2, level: "error")
                    try #require(!errors.isEmpty)
                    #expect(errors.contains { $0.body.contains(app.marker("stderr", before)) })
                    #expect(errors.allSatisfy { !$0.body.contains("e2e stdout") })
                    #expect(errors.allSatisfy { !$0.body.contains(other.app) })

                    let after = try await app.response(cli, retryStartup: false)
                    #expect(after.instance == before.instance)
                    #expect(after.requests > before.requests)
                    let otherAfter = try await other.response(cli, retryStartup: false)
                    #expect(otherAfter.instance == otherBefore.instance)
                    #expect(otherAfter.requests > otherBefore.requests)
                }
            }
        }
    }

    /**
     A one-batch matching tail includes recent app output and excludes older
     app output still present in a larger replay. A batch may contain several
     records, so `--tail 1` does not promise exactly one JSON line.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `limits stored replay to the recent matching batch`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                let early = try await app.response(cli)
                // Prove the early batch already exists before producing the
                // recent request, without assuming aggregation timing.
                _ = try await app.requireReplay(cli, containing: early)
                let recent = try await app.response(cli)
                let history = try await app.requireReplay(cli, containing: recent)
                #expect(history.contains { $0.body.contains(app.marker("stdout", early)) })
                #expect(history.contains { $0.body.contains(app.marker("stderr", recent)) })
                let tail = try await app.logs(cli, tail: 1)
                try #require(!tail.isEmpty)
                #expect(
                    tail.contains {
                        $0.body.contains(app.marker("stdout", recent))
                            || $0.body.contains(app.marker("stderr", recent))
                    }
                )
                #expect(
                    tail.allSatisfy {
                        !$0.body.contains(app.marker("stdout", early))
                            && !$0.body.contains(app.marker("stderr", early))
                    }
                )
                let after = try await app.response(cli, retryStartup: false)
                #expect(after.instance == early.instance)
                #expect(after.requests > recent.requests)
            }
        }
    }

    /**
     Displays usage for `wendy device logs`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device logs --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Stream logs from containers on WendyOS or the firmware console on Wendy Lite"
                    )
                )
                #expect(result.stdout.contains("wendy device logs [app] [flags]"))
                #expect(result.stdout.contains("--app"))
                #expect(result.stdout.contains("--service"))
                #expect(result.stdout.contains("--level"))
                #expect(result.stdout.contains("--tail"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the target device hostname and skips discovery and
     pickers. The command does not read or change the saved default device when
     an explicit target is supplied.
     */
    @Test(
        .disabled(
            "WDY-1952: explicit-target log streaming needs a seeded managed agent with telemetry state."
        )
    )
    func `uses explicit device selection without prompting`() async throws {}

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test
    func `reports missing device selection in non-interactive mode`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device logs Example --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no device specified"))
                #expect(!result.stderr.contains("Select a device"))
            }
        }
    }

    /**
     Connection failures, timeouts, and incompatible agent responses produce
     stderr diagnostics and a failure status. Output does not claim that the
     operation succeeded.
     */
    @Test(
        .disabled(
            "WDY-1952: connection and incompatible-RPC failures need controllable seeded managed-agent responses."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Streams logs from the selected device and applies app, service, level, and
     severity filters before presenting entries.
     */
    @Test(
        .disabled(
            "WDY-1952: filtered log streaming needs seeded managed-agent container and telemetry records."
        )
    )
    func `streams device logs`() async throws {}

    /**
     With `--json`, emits newline-delimited or array-wrapped structured log
     entries suitable for automation, without table formatting.
     */
    @Test(
        .disabled(
            "WDY-1952: JSON log framing needs seeded managed-agent telemetry records and bounded stream control."
        )
    )
    func `prints structured log entries in JSON mode`() async throws {}

    /**
     Cancelling log streaming closes the remote stream without changing app or
     device state.
     */
    @Test(
        .disabled(
            "WDY-1952: log cancellation cleanup needs a seeded stream and harness process control."
        )
    )
    func `shuts down cleanly on cancellation`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device logs --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
            }
        }
    }

    /**
     Rejects more positional arguments than the command's documented interface
     accepts.

     Validation fails before the requested cloud or device operation begins.
     */
    @Test
    func `rejects extra positional arguments`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device logs one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
