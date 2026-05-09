# WendyE2ETests

Swift E2E test package for the Wendy CLI and local Wendy agent. The long-term goal is to use this package as a behavioral specification suite, not just a collection of smoke tests.

## Running tests

From this package:

```bash
swift test --filter WendyE2ETests
```

For reproducible command records:

```bash
WENDY_E2E_TEST_RECORDS_DIR="$PWD/.build/e2e-test-records.current" \
  swift test --filter WendyE2ETests
```

To render the HTML report from the repository root:

```bash
.agents/skills/run-e2e-tests-and-analyze/render-e2e-report.py \
  --records-dir swift/WendyE2ETests/.build/e2e-test-records.current
```

## Behavioral spec workflow

Use this workflow when expanding E2E coverage for a command area.

1. Pick one bounded command area.
2. Write disabled Swift Testing stubs only.
3. Review the stubs as the product/API behavior spec.
4. Once agreed, implement the specs one by one.

The disabled stubs are the durable specification. They should describe externally observable behavior, not current implementation details.

Good first command areas are local and deterministic:

- `wendy json validate`
- `wendy project entitlements`
- `wendy cache`
- `wendy analytics`

When intentionally covering CLI-to-agent behavior, start with the smallest read-only interaction, such as `wendy device version`, before moving to commands that require browsers, hardware, streaming, cloud auth, deployment, or network discovery.

## Test organization and naming

Use flattened suites only. Each suite name is the full command phrase being specified; do not use nested suites. Test names complete the sentence.

```swift
@Suite(.serialized)
struct `'wendy help'` {
    @Test
    func `prints top-level help`() async throws {
        // TODO: implement.
    }

    @Test
    func `prints help for a nested command`() async throws {
        // TODO: implement.
    }
}
```

The rendered behavior reads as:

```text
wendy help prints top-level help
wendy help prints help for a nested command
```

For command variants, keep the flag in the test name when it is a mode of the same command:

```swift
@Suite(.serialized)
struct `'wendy info'` {
    @Test
    func `prints CLI and system details`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' prints CLI and system details as JSON`() async throws {
        // TODO: implement.
    }
}
```

Use a separate suite only when the variant reads better as its own command phrase, for example `'wendy --version'`.

Name files after command areas, not after our internal spec process. Prefer names like `WendyHelpTests.swift`, `WendyInfoTests.swift`, and `WendyAnalyticsTests.swift`; do not use `BehaviorSpec` in file names.

## Inline specification prose

Add a Markdown-capable `/** ... */` documentation block immediately before each `@Test`. The suite and test name form the heading; the block comment provides the prose context that a terse heading cannot capture.

Write this prose as present-tense product documentation, not as requirement language. Avoid words like "should", "must", "expect", or "will". The prose describes the desired end-state as if it is already true. Normative details belong in executable assertions, not in prose.

```swift
@Suite(.serialized)
struct `'wendy help'` {
    /**
     Prints the top-level help shown to users who ask for command discovery.

     This is the primary entry point for understanding the CLI. The output explains what Wendy is, groups related commands, shows global flags, and emits no stderr diagnostics because help is a successful informational command.

     Command group names and global flag names are part of the user-facing contract. Line wrapping is not part of the contract.
     */
    @Test
    func `prints top-level help`() async throws {
        let result = try await self.cli.sh("./bin/wendy help")

        #expect(result.terminationStatus.isSuccess)
        #expect(result.standardOutput?.contains("Project Commands:") == true)
        #expect(result.standardOutput?.contains("--json") == true)
        #expect(result.standardError?.isEmpty == true)
    }
}
```

The generated document should render this as:

```md
## wendy help prints top-level help

Prints the top-level help shown to users who ask for command discovery.

This is the primary entry point for understanding the CLI...
```

Prefer `/** ... */` over `///` for spec prose because it is easier to read, easier to parse as one block, and better suited to multi-paragraph Markdown. Reserve `///` for short API comments on helpers and support types.

## Executable requirements

The test body is the requirements layer. Assertions express the precise contract:

- exit status
- stdout and stderr behavior
- JSON shape and values
- filesystem/config side effects
- non-mutation on failure
- platform-specific behavior
- command records/evidence

As repeated patterns emerge, evolve the E2E DSL so requirements read naturally in code. The goal is not a decorative DSL; the goal is test bodies that read like executable requirements and fail with useful diagnostics.

Raw assertions are fine while a pattern is new:

```swift
#expect(record.terminationStatus.isSuccess)
#expect(record.standardError?.isEmpty == true)
#expect(record.standardOutput?.contains("Project Commands:") == true)
```

When a pattern repeats, prefer named helpers or DSL concepts:

```swift
try result.requiresSuccess()
try result.stdout.requiresContains("Project Commands:")
try result.stderr.requiresEmpty()
```

Future DSL directions include command success/failure helpers, stdout/stderr contracts, JSON shape assertions, file/config mutation assertions, help-section assertions, and platform gates.

## Spec stub style

Use disabled tests so unimplemented specs do not falsely pass:

```swift
/**
 Creates a minimal Swift WendyOS project in an empty directory.

 The command accepts app id, target, language, entitlements, and git choices,
 then writes the expected project files and concise success guidance.
 */
@Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
func `creates a minimal Swift WendyOS project non-interactively`() async throws {
    // TODO: implement.
}
```

A good spec stub:

- reads like product documentation
- names one user-visible behavior
- states setup, action, and expected outcomes
- identifies filesystem/config mutations and non-mutations
- avoids asserting incidental current wording unless the wording is itself the contract

## What to specify

For each command area, build a behavioral matrix before implementing test bodies:

- happy paths
- invalid input
- missing state
- existing state
- idempotency
- cancellation and prompts
- non-interactive behavior
- human output vs JSON output
- stdout/stderr contract
- exit status
- filesystem side effects
- config mutation and non-mutation
- analytics/environment isolation

For human-readable output, prefer semantic anchors. For JSON and config files, prefer exact structure and meaningful fields.

## Definition of a good implemented spec

An implemented E2E spec should be deterministic and hermetic where possible:

- use temporary project directories
- use temporary `HOME`/config directories
- avoid real browser, cloud, hardware, live device, network, and clock dependencies unless explicitly under test
- assert exit status
- assert stdout/stderr behavior
- assert relevant file/config side effects
- assert no partial mutation on failure

Avoid broad assertions like:

```swift
#error contains domain-specific text || error contains "Could not connect"
```

Those are acceptable only for rough smoke coverage, not for a behavioral spec.

## Current recommended starting point

Start with `wendy device version`.

Phase: spec stubs only; do not implement test bodies yet.

Goal: enumerate the externally observable behavior of the smallest Wendy CLI to Wendy agent interaction:

- local macOS app-backed agent lifecycle requirements and gates
- explicit `--device` connection behavior
- human-readable version output
- `--json` output shape and prompt-free behavior
- `device info` alias behavior
- missing device selection in non-interactive contexts
- unreachable device diagnostics
- stdout/stderr contract
- exit status

After the stubs read like a complete product/API spec, implement them incrementally.

## Cross-session handoff prompt

In a future session, use:

> Read `swift/WendyE2ETests/README.md` and continue the behavioral spec workflow from the current recommended starting point. Do not implement test bodies until the disabled spec stubs are agreed.

## Machine and session overview

`Machine` is static metadata: identity, OS, tags, SSH target, and working directory. It does not run commands.

```swift
@Test(.enabled(if: Machine.cli.os == .linux))
func `uses linux behavior`() async throws {
    let cli = try await Session.begin(for: .cli)
    try await cli.sh("./bin/wendy --version")
    try await cli.end()
}
```

Known machines are declared as static properties:

```swift
Machine.current  // the test runner, tagged `.runner`
Machine.cli
Machine.agent
```

Predefined machine OS values are `.macOS`, `.linux`, `.windows`, and `.wendyOS`.
Use `WENDY_E2E_CLI_OS` or `WENDY_E2E_AGENT_OS` to override a known
machine's declared OS for a run.

`Session` is the runtime command executor for a machine:

```swift
let cli = try await Session.begin(for: .cli)
try await cli.sh("./bin/wendy --version")
```

Use `Session.with` when a spec needs cleanup-safe session lifetimes:

```swift
try await Session.with(.cli, .agent) { cli, agent in
    try await cli.sh("./bin/wendy --version")
    try await agent.sh("make build-dev")
}
```

Use `session.command(...).poll(...).run()` when a command needs DSL configuration before execution:

```swift
try await agent
    .command("nc -z 127.0.0.1 50051")
    .poll(until: .success)
    .run()
```

If `ssh` is omitted, sessions run commands locally. `Session.begin(for:verbose:)` enables command echoing for that session; `WENDY_E2E_VERBOSE=1` enables it globally.
