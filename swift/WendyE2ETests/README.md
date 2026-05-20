# WendyE2ETests

Swift E2E test package for the Wendy CLI and local Wendy agent. The long-term goal is to use this package as a behavioral specification suite, not just a collection of smoke tests.

## Running tests

From this package:

```bash
swift test --filter WendyE2ETests
```

From `swift/`, the helper script writes runner output under an explicit
output directory, builds the managed CLI into `go/bin` (or
`WENDY_E2E_CLI_BIN_DIR`), writes isolated CLI and agent sandboxes, and captures
Swift Testing results and command recordings:

```bash
bash Scripts/E2ETest.sh --output-dir Build/e2e
```

For the common local workflow that aggregates the raw run, renders the aggregate `index.html`, and opens it on macOS:

```bash
make e2e-run
```

For reproducible command recordings when invoking SwiftPM directly:

```bash
RUN_ID="current"
RUN_DIR="$PWD/.build/e2e/$RUN_ID"
CLI_RUN_DIR="$HOME/.wendy/e2e/$RUN_ID/cli"
CLI_BIN_DIR="$PWD/../../go/bin"
AGENT_RUN_DIR="$HOME/.wendy/e2e/$RUN_ID/agent"
rm -rf "$RUN_DIR" "$CLI_RUN_DIR" "$AGENT_RUN_DIR"
mkdir -p "$CLI_BIN_DIR"
(cd ../../go && go build -o "$CLI_BIN_DIR/wendy" ./cmd/wendy)
WENDY_E2E_RUN_ID="$RUN_ID" \
WENDY_E2E_RUN_DIR="$RUN_DIR" \
WENDY_E2E_CLI_RUN_DIR="$CLI_RUN_DIR" \
WENDY_E2E_CLI_BIN_DIR="$CLI_BIN_DIR" \
WENDY_E2E_AGENT_RUN_DIR="$AGENT_RUN_DIR" \
WENDY_E2E_ISOLATION=per-run \
swift test --filter WendyE2ETests
```

Each implemented test writes recordings under
`<run-dir>/tests/<suite-file-stem-dasherized>/<test-name-dasherized>/`, where
the suite file stem is the test file name with the `Tests` suffix removed. For
example, `WendyDeviceInfoTests.swift` records under
`wendy-device-info/<test-name>/`. This keeps per-suite space available for
suite-level artifacts while each test retains its own recording directory. The
`recording.sh.txt` file replays the captured `sh()` invocations in order for
manual debugging while remaining browser-viewable from the HTML report.

Sandbox isolation is controlled by `--isolation` or `WENDY_E2E_ISOLATION`:

- `per-test` (default): one sandbox per role under each test recording directory. This is required for `--parallel`.
- `per-run`: one stable `home/`, `tmp/`, and `home/work` sandbox per role. In non-parallel runs, the role sandbox is reset before each test's first command.
- `none`: no synthetic `HOME`, `TMPDIR`, or working directory is configured, and existing machine state is left untouched.

To render the aggregate HTML report from this package:

```bash
swift run swift-e2e-testing report --run-dir /tmp/wendy/<workflow-name>.<run-id>
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

Use one flattened suite per E2E test file. The suite name is the full command phrase being specified; do not use nested suites. Test names complete the sentence.

Derive the file name from the suite name using PascalCase. For example, this suite maps to the command stem `WendyDeviceInfo` and, with the package's SwiftPM test-file suffix, lives in `WendyDeviceInfoTests.swift`:

```swift
@Suite(.serialized)
struct `'wendy device info'` {
    @Test
    func `prints JSON device information`() async throws {
        // TODO: implement.
    }
}
```

The rendered behavior reads as:

```text
wendy device info prints JSON device information
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

Use a separate file and suite only when the variant reads better as its own command phrase, for example `'wendy --version'`.

Name files after command areas, not after our internal spec process. Prefer names like `WendyHelpTests.swift`, `WendyInfoTests.swift`, and `WendyAnalyticsTests.swift`; do not use `BehaviorSpec` in file names.

## Scenarios and test lifecycle

Do not put E2E setup or teardown in suite `init` or `deinit`. Start sessions from an `@Test` body, or from a helper that forwards `filePath`, `function`, and `line` defaults from the test call site.

Use scenarios for setup and teardown instead. A scenario is the lifecycle boundary for a test: it can perform async setup before yielding sessions, run the test body, and perform async teardown afterward. That matters because suite initialization is not a reliable test-body call site for the E2E harness, and because `deinit` is always infallible and non-async. The harness needs the actual `@Test` call site to derive the per-test sandbox and recording paths.

Scenarios also centralize the test harness "magic": they attach the recorder, select the managed `wendy` binary through `PATH`, assign isolated `HOME`, `TMPDIR`, and working directories, and keep command recordings tied to the test that produced them. Starting sessions from suite initialization bypasses that identity and can create shared sandboxes or misleading records.

```swift
@Suite(.serialized)
struct `'wendy device info'` {
    private let scenario = CLIAndAgentScenario()

    @Test
    func `prints JSON device information`() async throws {
        try await self.scenario.run { cli, agent in
            // Test commands go here.
        }
    }
}
```

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
        try await self.cli.sh("./bin/wendy help") { result in
            #expect(result.status.isSuccess)
            #expect(result.stdout.contains("Project Commands:"))
            #expect(result.stdout.contains("--json"))
            #expect(result.stderr.isEmpty)
        }
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
- command recordings/evidence

As repeated patterns emerge, evolve the E2E DSL so requirements read naturally in code. The goal is not a decorative DSL; the goal is test bodies that read like executable requirements and fail with useful diagnostics.

Raw assertions are fine while a pattern is new:

```swift
#expect(result.status.isSuccess)
#expect(result.stderr.isEmpty)
#expect(result.stdout.contains("Project Commands:"))
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

`WendyE2EMachine` is static metadata: identity, OS, tags, optional SSH user/address, and working directory. It does not run commands.

```swift
@Test(.enabled(if: WendyE2EMachine.cli.os == .linux))
func `uses linux behavior`() async throws {
    let cli = try await WendyE2ESession.begin(for: .cli)
    try await cli.sh("./bin/wendy --version")
    try await cli.end()
}
```

Known machines are declared as static properties:

```swift
WendyE2EMachine.current  // the test runner, tagged `.runner`
WendyE2EMachine.cli
WendyE2EMachine.agent
```

Predefined machine OS values are `.macOS`, `.linux`, `.windows`, and `.wendyOS`.
Use `WENDY_E2E_CLI_OS` or `WENDY_E2E_AGENT_OS` to override a known
machine's declared OS for a run.

`WendyE2ESession` is the runtime command executor for a machine:

```swift
let cli = try await WendyE2ESession.begin(for: .cli)
try await cli.sh("./bin/wendy --version")
```

Use `WendyE2ESession.with` when a spec needs cleanup-safe session lifetimes:

```swift
try await WendyE2ESession.with(.cli, .agent) { cli, agent in
    try await cli.sh("./bin/wendy --version")
    try await agent.sh("make agent-build")
}
```

Use `sh(...)` for shell commands. The no-callback form requires the command to succeed; the callback form receives the full shell result for assertions:

```swift
try await agent.sh("nc -z 127.0.0.1 50051")

try await agent.sh("nc -z 127.0.0.1 50051") { result in
    #expect(result.status.isSuccess)
    #expect(result.stderr.isEmpty)
}
```

When a command needs shell-specific syntax, pass both variants and `sh` selects the one matching the machine OS:

```swift
try await agent.sh(
    posix: "nc -z 127.0.0.1 50051",
    power: "Test-NetConnection -ComputerName 127.0.0.1 -Port 50051"
)
```

Sessions run locally when `address` is omitted. If `address` is provided, commands run over SSH; `user` is included in the SSH target when provided. Local sessions still execute commands through a shell and honor configured working directories and environment. `WendyE2ESession.begin(for:verbose:)` enables command echoing for that session; `WENDY_E2E_VERBOSE=1` enables it globally.
