# WendyE2ETests

Swift end-to-end tests for the Wendy CLI and Wendy agent. This package runs the
real `wendy` binary, records every shell command, and writes artifacts for local
debugging, CI, and AI review.

The harness is currently separate from the `.github/ci-tests/` integration
suite. Over time, overlapping integration coverage is expected to move into
Swift E2E specs so orchestration, assertions, artifacts, and reporting can live
in one place.

## Quick start

Building the CLI requires Node.js 22 and npm on the CLI machine's `PATH` for
the embedded UI. CI installs them on the runner. If `WENDY_E2E_CLI_ADDRESS`
selects a remote CLI host, install them there and make them available to its
SSH login shell. The harness checks this before building.

From `swift/`:

```bash
make e2e-test
make e2e-analyze
make e2e-reference
```

`make e2e-test` builds the managed CLI into `go/bin`, runs the Swift E2E tests
locally, and writes attempt artifacts under `Build/e2e`. It expects a Wendy
agent to already be running on the local machine. `make e2e-analyze` aggregates
attempts, runs the AI review step, renders the HTML report, and opens it locally
when supported. `make e2e-reference` renders reference documentation from the
E2E test source and opens it locally when supported.

You can also run only the individual analysis stages:

```bash
make e2e-aggregate
make e2e-review
make e2e-report
```

From this package, run the test target directly when you do not need the full wrapper:

```bash
swift test --filter WendyE2ETests
```

## Common workflows

### Run a single test or suite

From `swift/`:

```bash
bash Scripts/E2ETest.sh \
  --output-dir ../Build/e2e \
  --filter "wendy device info"
```

Repeat `--filter` to pass multiple test filters.

### Test against a remote device

From `swift/`:

```bash
make e2e-test-wendy DEVICE=wendyos-raspberry-pi-5.local
make e2e-test-linux DEVICE=my-linux-box.local
make e2e-test-macos DEVICE=mac-mini.local
```

### Run legacy app integration tests

The `legacy integration tests` suite preserves parity with `go/scripts/test-ci.sh`
by deploying fixtures from `.github/ci-tests/`. It is skipped by default because
it requires Docker/buildx and a real WendyOS target. In CI, set
`WENDY_E2E_LEGACY_INTEGRATION` only in protected, non-fork workflows: the suite
deploys apps to a real device, so it must never run against untrusted pull
requests.

```bash
WENDY_E2E_LEGACY_INTEGRATION=true \
  bash Scripts/E2ETest.sh \
    --output-dir ../Build/e2e \
    --filter "legacy integration tests"
```

## Test environment

`Scripts/E2ETest.sh` is the preferred runner. It:

- builds the Go CLI into `go/bin` or `WENDY_E2E_CLI_BIN_DIR`
- creates isolated CLI and agent run directories
- puts the managed `wendy` binary first on `PATH`
- passes machine metadata into the Swift tests
- writes xUnit output, command recordings, replay scripts, AI review Markdown,
  and HTML reports

The most useful environment variables are:

| Variable | Purpose |
| --- | --- |
| `WENDY_E2E_RUN_ID` | Explicit run/attempt identifier. |
| `WENDY_E2E_OUTPUT_DIR` | Root directory consumed by `Scripts/E2ETest.sh`. |
| `WENDY_E2E_RUN_DIR` | Run directory consumed by the Swift test harness. |
| `WENDY_E2E_TEST_FILTERS` | Comma-separated test filters. |
| `WENDY_E2E_CLI_RUN_DIR` / `WENDY_E2E_AGENT_RUN_DIR` | Role run directories consumed by scenarios. |
| `WENDY_E2E_CLI_BIN_DIR` | Directory containing the managed `wendy` binary. |
| `WENDY_E2E_CLI_AUTH_CONFIG_PATH` | Dedicated Wendy CLI auth config fixture for authenticated tests. |
| `WENDY_E2E_CLI_ADDRESS` | Optional SSH host for the CLI machine. |
| `WENDY_E2E_AGENT_ADDRESS` | Optional SSH host for the agent/device machine. |
| `WENDY_E2E_DEVICE_ADDRESS` | Device address used by CLI commands while the agent session still runs locally; must not include credentials. |
| `WENDY_E2E_MANAGED_AGENT` | Build and launch a local `wendy-agent` process for the run. macOS managed-agent runs use the real WendyAgentMac app so native file sync and app lifecycle coverage matches production Mac behavior; Linux/WendyOS managed-agent runs continue to use the Go daemon. |
| `WENDY_E2E_CLI_OS` / `WENDY_E2E_AGENT_OS` | Override machine OS metadata. |
| `WENDY_E2E_ISOLATION` | Sandbox mode: `per-test`, `per-run`, or `none`. |
| `WENDY_E2E_PARALLEL` | Enables parallel test execution when supported by the runner. |
| `WENDY_E2E_TEST_TIMEOUT_SECONDS` | SwiftPM test process timeout in seconds; 0 disables. |
| `WENDY_E2E_VERBOSE` | Print every machine command before it runs. |

Sandbox isolation modes:

- `per-test` (default): each test gets separate CLI and agent sandboxes. Use
  this for parallel-safe runs.
- `per-run`: tests share stable role sandboxes for the run. In serial runs, each
  role sandbox is reset before a test's first command.
- `none`: the harness does not override `HOME`, `TMPDIR`, or working directory.
  Existing machine state is used directly.

Authenticated scenarios copy `WENDY_E2E_CLI_AUTH_CONFIG_PATH` into the test
sandbox. Prefer a dedicated E2E fixture config instead of your live
`~/.wendy/config.json`; do not commit fixture configs, and avoid putting
credential-related values directly in shell history. If the variable is not set,
the runner uses the current CLI user's `~/.wendy/config.json` where possible.

## Artifacts

The E2E workflow uses two artifact layouts: attempt directories from individual
test runs, and aggregate run directories that keep attempt-level artifacts
separate from per-test observations.

### Attempt directory

`Scripts/E2ETest.sh` writes one attempt directory under the output directory.
Attempt-level artifacts stay at the attempt root. Per-test observation
directories use the test file stem without the `Tests` suffix, dasherized; test
directories use the dasherized test name.

```text
<output-dir>/<attempt-id>/
├── attempt.json
├── test-results.xml
├── test-results.raw.xml          # only when XML sanitization changed the file
└── observations/
    └── <suite>/<test>/
        ├── recording.md
        ├── recording.sh.txt
        └── test.json
```

The attempt ID has the shape:

```text
<workflow-name>.<run-id>.<target-name>.<attempt-number>
```

For example:

```text
swift-e2e-tests.local0000.macos-15-to-wendyos-raspberry-pi-5.0001
```

### Aggregate run directory

`make e2e-aggregate` or `Scripts/E2EAggregate.sh` maps one or more attempt
directories into a run directory:

```text
<output-dir>/<workflow-name>.<run-id>/
├── attempts/
│   └── <target-name>/<attempt-number>/
│       ├── attempt.json
│       ├── test-results.xml
│       └── test-results.raw.xml  # only when present in the source attempt
├── source-index.md
└── observations/
    └── <suite>/<test>/
        ├── source.md
        ├── test.json
        └── <target-name>/<attempt-number>/
            ├── recording.md
            ├── recording.sh.txt
            └── test.json
```

The `attempts/` tree keeps every attempt-root artifact except `observations/`,
so preflight logs and metadata remain attached to the attempt. Aggregate runs
also copy `test.json` to the test root and write `source.md`, which contains the
extracted test source range including the DocC/spec comment above `@Test` when
present. `source-index.md` lists those source artifacts for AI and human review.

`make e2e-review` runs a single AI review pass and writes run-level issue files
into the aggregate run directory:

```text
<run>/review.<reviewer>/<slug>.md
```

`make e2e-report` writes the rendered report files at the aggregate run root:

```text
<run>/index.html
<run>/review.md
<run>/review.html
```

`recording.md` is the human-readable command log. `recording.sh.txt` replays the
captured `sh()` calls in order for manual debugging. `source.md` is kept
separate from recordings so runtime transcripts stay focused while review still
has the spec/test source that produced them.

AI review files are Markdown. Top-level `review.md` is the compact aggregate
that can be posted as a CI comment. Attempt, overview, and AI review JSON
schemas live in `Support/Schemas/`.

### Reference directory

`make e2e-reference` renders static reference documentation from the suite and
test documentation comments, independent of any test run. It writes local files
and opens `index.html`; it does not start a web server or network listener.

```text
Build/Reference/
├── index.html
└── <suite>.html
```

Use this artifact to review the behavioral reference generated from the current
E2E source files.

## Writing tests

For CLI contributions, read the [CLI design guidance](../../go/internal/cli/assets/docs/development/contributing.md#cli-design).

### Organization and naming

Use one flattened suite per command area, and keep exactly one `@Suite` per
source file. The suite name is the command phrase; the test name completes the
behavior sentence. Put related aliases or deprecated command areas in their own
test files rather than adding a second suite to an existing file. Inside
backticked Swift suite or test identifiers, wrap command fragments, aliases, and
flags that should render as code with single quotes. The reference extractor
converts those quoted spans to Markdown code spans (and HTML `<code>` elements).
Use `'... subcommand'` when referring to a related command under the same suite
prefix.

When the same behavior is specified for multiple related commands, keep the same
suite structure, section order, and test-name wording wherever possible. Change
only the quoted command fragment or the smallest phrase needed to distinguish the
command path.

```swift
// WendyDeviceInfoTests.swift
@Suite
struct `'wendy device info'` {
    @Test
    func `prints JSON device information`() async throws {
        // Test body.
    }

    @Test
    func `'--json' reports a missing device without prompting`() async throws {
        // Test body.
    }
}

// WendyDevicePsTests.swift
@Suite
struct `'wendy device ps'` {
    @Test
    func `aliases '... device apps list'`() async throws {
        // Test body.
    }
}
```

This renders as:

```text
wendy device info prints JSON device information
```

Name test files exactly as the suite inside them using PascalCase plus
`Tests.swift`:

```text
WendyDeviceInfoTests.swift
WendyAnalyticsStatusTests.swift
WendyJsonValidateTests.swift
```

Keep command-mode flags in the test name when they belong to the same command:

```swift
@Suite
struct `'wendy info'` {
    @Test
    func `prints CLI and system details`() async throws {}

    @Test
    func `'--json' prints CLI and system details as JSON`() async throws {}
}
```

Use a separate suite only when the variant reads better as its own command, such
as `wendy --version`.

### Scenarios

Use scenarios for setup and teardown. Do not start E2E sessions in suite `init`
or `deinit`; the harness needs the `@Test` call site to choose the right sandbox
and recording paths.

```swift
@Suite
struct `'wendy device info'` {
    let scenario = CLIAndAgentScenario()

    @Test
    func `prints JSON device information`() async throws {
        try await self.scenario.run { cli, agent in
            let agentAddress = agent.machine.address

            try await cli.sh("wendy --json --device \(agentAddress) device info") { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.isEmpty)
            }
        }
    }
}
```

`CLIAndAgentScenario` creates CLI and agent sessions, attaches the recorder,
installs the managed CLI on `PATH`, configures isolated `HOME` and `TMPDIR`, and
copies the auth fixture for authenticated tests. For scenario-specific target
setup or cleanup, pass `before` and/or `after` hooks to the scenario initializer;
`after` runs even when setup or the test body fails, and the original failure is
preserved unless cleanup is the only failure.

```swift
let scenario = CLIAndAgentScenario(
    before: { _, agent in
        try await agent.sh("brew uninstall --force hello || true")
    },
    after: { _, agent in
        try await agent.sh("brew uninstall --force hello || true")
    }
)
```

### Specification prose

Add a Markdown-capable `/** ... */` block immediately before each `@Test`. Write
it as concise product documentation for the behavior under test.

```swift
/**
 Prints the top-level help shown to users who ask for command discovery.

 The output explains what Wendy is, groups related commands, shows global flags,
 and emits no stderr diagnostics because help is a successful informational
 command.
 */
@Test
func `prints top-level help`() async throws {
    // Assertions go here.
}
```

Use the test body for precise requirements: exit status, stdout/stderr, JSON
shape, filesystem changes, config mutations, and failure behavior.

### Assertions

Prefer direct, useful assertions while a pattern is new:

```swift
#expect(result.status.isSuccess)
#expect(result.stderr.isEmpty)
#expect(result.stdout.contains("Project Commands:"))
```

When a pattern repeats, move it into a named helper so test bodies read like
executable requirements.

### Disabled specs

Use disabled tests for agreed behavior that has not been implemented yet:

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

A good disabled spec names one user-visible behavior and captures the important
setup, action, output, and side effects.

## Session API reference

`WendyE2EMachine` describes a command target: `id`, `name`, `os`, tags,
`isLocal`, optional SSH user, and resolved address. Known machines are:

```swift
WendyE2EMachine.current
WendyE2EMachine.cli
WendyE2EMachine.agent
```

`WENDY_E2E_CLI_OS` and `WENDY_E2E_AGENT_OS` override the known machines' OS
metadata. Supported values are macOS, Linux, Windows, and WendyOS.

`WendyE2ESession` runs commands on a machine:

```swift
let cli = try await WendyE2ESession.begin(for: WendyE2EMachine.cli)
try await cli.sh("wendy --version")
try await cli.end()
```

Use `WendyE2ESession.with` for cleanup-safe lifetimes:

```swift
try await WendyE2ESession.with(
    WendyE2EMachine.cli,
    WendyE2EMachine.agent
) { cli, agent in
    try await cli.sh("wendy --version")
    try await agent.sh("nc -z 127.0.0.1 50051")
}
```

The single-string forms of `sh` and `pty` use the same command text for POSIX
shells and PowerShell. Use them only when the command is portable across the
machines under test.

The no-callback forms of `sh` and `pty` require the command to succeed. Use the
callback form when a command may fail or needs assertions:

```swift
try await cli.sh("wendy --json device info") { result in
    #expect(result.status.isSuccess)
    #expect(result.stderr.isEmpty)
}
```

Use `pty` for commands whose behavior depends on having an interactive terminal:

```swift
try await cli.pty("wendy device info") { result in
    #expect(result.status.isSuccess)
}
```

For OS-specific shell syntax, provide both variants. The session chooses `posix`
for macOS, Linux, and WendyOS machines, and `power` for Windows machines:

```swift
try await agent.sh(
    posix: "nc -z 127.0.0.1 50051",
    power: "Test-NetConnection -ComputerName 127.0.0.1 -Port 50051"
)
```

The variant form also supports callbacks:

```swift
try await cli.sh(
    posix: "printf 'hello\\n'",
    power: "Write-Output hello"
) { result in
    #expect(result.normalizedStdout == "hello")
}
```

`WendyE2ESession.wendyCacheDirectory` returns the Wendy cache path for the
session's machine OS and environment.

Sessions run locally when the machine is local. If a machine is created or
configured with an address, commands run over SSH and include the configured
user when present.

## Direct test invocation

When bypassing `Scripts/E2ETest.sh`, set the same environment the wrapper would
normally provide:

```bash
RUN_ID="current"
RUN_DIR="$PWD/.build/e2e/$RUN_ID"
CLI_RUN_DIR="$HOME/.wendy/e2e/$RUN_ID/cli"
CLI_BIN_DIR="$PWD/../../go/bin"
# Use a dedicated E2E auth fixture, not your live ~/.wendy/config.json.
CLI_AUTH_CONFIG_PATH="$HOME/.wendy/e2e-config.json"
AGENT_RUN_DIR="$HOME/.wendy/e2e/$RUN_ID/agent"

rm -rf "$RUN_DIR" "$CLI_RUN_DIR" "$AGENT_RUN_DIR"
mkdir -p "$CLI_BIN_DIR"
(cd ../../go && go build -o "$CLI_BIN_DIR/wendy" ./cmd/wendy)

WENDY_E2E_RUN_ID="$RUN_ID" \
WENDY_E2E_RUN_DIR="$RUN_DIR" \
WENDY_E2E_CLI_RUN_DIR="$CLI_RUN_DIR" \
WENDY_E2E_CLI_BIN_DIR="$CLI_BIN_DIR" \
WENDY_E2E_CLI_AUTH_CONFIG_PATH="$CLI_AUTH_CONFIG_PATH" \
WENDY_E2E_AGENT_RUN_DIR="$AGENT_RUN_DIR" \
WENDY_E2E_ISOLATION=per-run \
swift test --filter WendyE2ETests
```

Prefer the wrapper for normal development; use this only when debugging the
harness itself. Authenticated tests copy `CLI_AUTH_CONFIG_PATH` into sandbox
directories, so keep the fixture out of version control and remove stale
sandboxes when they are no longer needed.
