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

The first-app Python scaffold checks (default parsed build settings and optional
installation) require Python 3.11+ on the CLI machine. They check this prerequisite
before scaffolding and report how to select a supported `python3` on `PATH`.
CI selecting either check must provision that interpreter. The optional installation
check additionally needs an offline wheelhouse and venv/ensurepip tooling.
Provision the wheelhouse once in an isolated fixture directory, then pass its absolute path
on the CLI machine as `WENDY_E2E_PYTHON_WHEELHOUSE`:

```bash
python3 -m pip download --no-cache-dir --only-binary=:all: \
  --dest /path/to/isolated/wheelhouse hatchling==1.27.0
WENDY_E2E_PYTHON_WHEELHOUSE=/path/to/isolated/wheelhouse \
  bash Scripts/E2ETest.sh --output-dir /path/to/evidence \
    --no-managed-agent --filter "installs and runs the generated Python"
```

The spec creates a per-test virtual environment, installs with `--no-index`, and
runs the installed console script outside the source tree. Without that fixture,
only this installation spec is skipped; parsed build settings remain covered.

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

### Run the HTTP app lifecycle specs

`WENDY_E2E_HTTP_APP=true` opts into uniquely named real-device app fixtures
from `.github/ci-tests/http-app-loop`. Select an explicit WendyOS device and
an isolated CLI HOME. This fixture accepts plain dotted LAN hostnames or IPv4
addresses only; it rejects cloud/provider/VM/socket selectors, explicit ports,
credentials and unsupported IPv6 forms before scenario setup or inventory.
The CLI host needs Python 3, curl, and OpenSSL. Before
any Wendy command, the fixture reduces the copied single-context auth config
to its held certificate/key and saved device pins. It removes OAuth/API tokens,
refresh tokens, DPoP/service-account material and renewal endpoints, then checks
the actual X509 validity locally. Expired or not-yet-valid certificates fail
before device access. Remaining signed validity must exceed the maximum
15-minute CLI renewal margin plus the enforced test timeout and 60 seconds of
runner shutdown grace. These fixtures require `--test-timeout` between 1 and
300 seconds; the runner exports its actual normalized bound. An ambient
`WENDY_AGENT_SOCKET` is refused before scenario setup because it overrides the
explicit device. No test refreshes a copied session. Private source auth files
remain unchanged.

The prebuilt Compose cases pin a small official Python multiarch image by
immutable digest, then run the HTTP payload through Compose's `command` array.
They need no local or remote builder. Cleanup removes only the exact app;
the public immutable image remains shared device cache content and is never
deleted by the fixture. Runs explicitly skip Cloud app registration and never
consent to changing the builder role. Every fixture CLI command sets the
supported `WENDY_RUN_NO_CLOUD_FALLBACK=1` setting; failed primary LAN resolution
must not query Cloud. Build-host resolution is explicitly exempt from that
setting, so both remote-builder negative specs are deferred with full bodies.
No product routing or security behavior is changed by this harness setup.

```bash
WENDY_E2E_HTTP_APP=true bash Scripts/E2ETest.sh \
  --no-managed-agent --agent-os wendyos --test-timeout 300 \
  --device-address my-device.local \
  --cli-auth-config /private/path/e2e-auth.json \
  --filter "prebuilt HTTP"
```

The HTTP assertions verify app identity and payload version, a live reported
endpoint, inventory identity/state/port, endpoint closure after stop, and a new
process instance after restart. Detached start must report unchecked readiness.
A separate full-body disabled spec retains companion version preservation:
Compose currently reports `latest` instead of configured `1.0.0` in inventory.
Other Compose cases leave that assertion to the disabled defect spec. To replay
that known failure, explicitly set both `WENDY_E2E_REPLAY_KNOWN_DEFECTS=true` and
`WENDY_E2E_HTTP_APP=true` and filter `preserves the companion version`.
The replay retains the original `1.0.0` assertion.

The everyday update/log batch uses the same prebuilt service. Select the exact
test-name filters `replays owned stdout`, `limits stored replay`,
`keeps serving after output`, `redeploys changed HTTP`, and
`preserves the working HTTP`. The replay cases compare actual request markers
from both streams, app and severity filtering, and recent versus older matching
batches. `--tail` counts batches, not JSON lines. The pressure case writes a
bounded 2 MiB per stream without a log reader and requires the same process to
keep serving HTTP. It does not prove unlimited output handling or attribution
of Agent events from a crash-loop that emits no stdout; the latter remains
uncovered. Changed runtime configuration must change the served payload version
and process while preserving a second owned app. Invalid-manifest preservation
is checked before deployment, followed by a repaired successful redeploy; it
does not assert rollback after an accepted replacement starts or crashes.

Five source-build positive specs retain complete bodies but remain disabled:
the available test Jetson lacks BuildKit and the CLI host has no container
builder. Their same-device `--build-host` path also rejects `--wait-ready`.
No fixture installs or enables a builder. A claimed ready result must satisfy
the declared TCP probe immediately; only unchecked detached startup may retry.
If source deployment may have imported an image but no container exists,
cleanup fails loudly with the exact app/repository because app inventory alone
cannot establish image absence. It never prunes shared device caches.

The same opt-in allows selective real-device checks filtered by
`rejects a missing application and preserves`. The unavailable-builder and
unsupported remote-readiness cases remain deferred because the supported
no-fallback setting exempts build hosts. Use a trusted hostname or saved
identity binding; a bare IP may be refused by identity verification.

For the offline no-Cloud contact control, point `WENDY_E2E_CONTROL_CLI` at a
locally built CLI and run `test_lan_no_cloud.py` with Python's unittest runner.
It uses only synthetic credentials, an unreachable loopback Agent, and a local
Cloud contact counter, including an unset-guard positive control.

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

For repeated explicit-device inner loops with the Bash runner, add
`--device-cache-fixture /absolute/path/to/e2e-target-cache.json` (or
`WENDY_E2E_CLI_DEVICE_CACHE_FIXTURE`) alongside `--device-address`. The first
run prepares target-only, untrusted `devices.json` address hints after a successful
`device info --read-only` preflight in a temporary HOME. The supplied auth fixture
must already contain a saved device principal for the explicit target hostname.
The CLI parses and enforces that existing binding on every connection; cache
metadata never creates or replaces identity bindings. In particular, a cached
`mtls: true` value is a routing hint, not proof of an authenticated connection.
Unset `WENDY_AGENT_SOCKET`: the runner, preflight and opted-in scenario reject
this ambient override because it bypasses the explicit device address.
Preparation pays the normal cold lookup cost. Later runs seed that preflight
HOME from the prepared fixture and check reachability using the real CLI and
its mandatory saved binding. Each run writes a separate refreshed snapshot;
tests copy it into their own HOME. No TLS session cache, pin store or additional
credentials are copied.
The preflight command has a separate 30-second deadline; `--test-timeout` applies
to the subsequent Swift test process.

Only scenario calls with `run(useDeviceCache: true)` consume that snapshot;
currently the explicit-device and non-interactive device-info specs opt in.
Discovery and other scenarios remain cold by default. This requires `per-test`
isolation and an unmanaged explicit target. The prepared fixture is immutable
and bound to the exact target argument. Use a named LAN target that the CLI
persists in its discovery cache; IP literals do not produce these entries.
Missing fixtures are prepared; malformed or mismatched fixtures fail setup.
The CLI retains its normal stale-address
fallback. Remove a stale fixture to prepare a fresh one rather than repeatedly
paying for fallback. Use a dedicated path, never your personal
`~/.wendy/devices.json`. Do not commit fixture files. The PowerShell runner does
not prepare these fixtures.

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

`swift-e2e-testing reference --spec-review` (or `--include-disabled-state`)
adds static availability metadata in HTML, Markdown and JSON. `enabled` means
no recognized availability trait restricts the declaration; it does not mean the
test passed or its runtime prerequisites are satisfied. Unconditional `.disabled`
is `disabled`. `.enabled(if:)`, conditional `.disabled` and
closure conditions are `conditional`: the generator never evaluates Swift
expressions, even literal booleans. Suite traits are inherited. An unconditional
disable takes precedence over conditional traits. Trait source is retained as
`availabilityReason`, including any literal explanation, without evaluating
interpolation or computed reasons. Predicate overloads, including a reason followed
by a closure or predicate expression, remain conditional. Ordinary reference output
still omits metadata. Generic argument lists inside traits are not parsed:
commas in an expression such as `.disabled(Reason<Int, String>.comment)` can be
reported conservatively as `conditional` (unknown), even when Swift resolves the
expression to an unconditional comment. A parenthesized generic expression is
supported because its commas are grouped. Other supported computed comments remain
unconditional; the extractor does not typecheck or evaluate their expressions.

For API consumers, `Entry.availability` and `Entry.availabilityReason` are additive.
The existing `isDisabled` initializer/property remains available; false alone is
not evidence of enablement. Setting that property explicitly resets availability
and clears its reason. JSON with availability metadata adds `availability` and
`availabilityReason` (when present). It retains `isDisabled` for static enabled or
disabled entries, and omits that legacy boolean for conditional entries so they
cannot be mistaken for enabled. Consumers requiring that key must migrate to
`availability` before reading conditional entries.

The extractor follows the repository convention: standalone `@Suite` / `@Test`
attributes followed by backticked `struct` / `func` names, one flattened suite per
command file. Its small lexical scanner balances function bodies and attributes
across ordinary, raw and multiline strings (including interpolation), quoted
identifiers, line comments and nested block comments. It also handles a function's
opening brace on a later line. This is not a complete Swift parser: regex literals,
inline attribute/declaration layouts, custom or qualified availability traits,
nested suite inheritance and conditional compilation are not interpreted. Keep
availability traits directly in the suite/test attribute when using this reference.

Recognized unterminated `@Test` / `@Suite` attributes fail file/directory parsing
and the CLI with a source path and line diagnostic. CLI directory output validates
all sources before writing any files. The existing nonthrowing `parseSource` API
returns no documents for this error, rather than a partial reference; use the
throwing `parseFile` API when diagnostics are needed. Other Swift syntax errors
are outside this bounded validation.

## Writing tests

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

## Auth Modes acceptance

Run the local and isolated tiers through the normal entry point (no board,
Cloud login, shared CLI config or running Agent is needed):

```bash
bash Scripts/E2ETest.sh --no-managed-agent --test-timeout 1200 \
  --filter 'wendy auth local-key show' --filter 'Auth Modes isolated acceptance'
```

The CLI subprocesses use separate config directories. The Swift scenario and
recorder capture Go's structured test events, actual CLI commands and diagnostics,
plus every method/caller admission decision. Missing selections, skips inside
an enabled isolated test, build/setup failures and unexpected failures are red.
Remote CLI and Windows tiers are explicitly skipped. Isolated Go compilation
uses a checkout-owned cache under `Build/auth-modes-go-cache`; `GOMODCACHE`
may select an existing module cache.

[AuthModesCoverage.json](AuthModesCoverage.json) maps all 31 approved scenarios to
implemented assertions and explicitly unfinished/external sub-assertions. It is
not a results file. The 207-row frozen matrix is independent of the runtime
permission table. Its 1,242 decisions prove admission to inert handlers, and
12 registered unmapped unary/stream calls prove refusal before entry. Neither
establishes physical operations. This PR's local run exercises no hardware method
rows. Every matrix row remains hardware-unverified by that run, even if its
isolated admission succeeds. Live CLI recordings establish the specific commands
and effects observed; they do not claim wire attribution for all 207 rows.

[WDY-4393 — Owner CLI can keep treating a reopened device as Closed after polling
during restart](https://linear.app/wendylabsinc/issue/WDY-4393/owner-cli-can-keep-treating-a-reopened-device-as-closed-after-polling)
runs by default on supported local platforms. Only the reproduced stale-pin
assertion is recorded as a known issue. A tool/setup failure or different failure
stays red. `WENDY_E2E_AUTH_MODES_STRICT=true` makes that functional failure red too.
The normal-entry enrollment test protects the fix in
[WendyOS #3071 — feat(cli): use, manage and install Closed devices](https://github.com/wendylabsinc/WendyOS/pull/3071)
for [WDY-4392 — CLI refuses to enroll a Closed device with its verified owner key](https://linear.app/wendylabsinc/issue/WDY-4392/cli-refuses-to-enroll-a-closed-device-with-its-verified-owner-key).
Its isolated Cloud intentionally denies enrollment authority after local ownership
passes. It never labels a partial synthetic enrollment as success.

Physical suites are nested under one serialized `AuthModesLiveAcceptance` parent;
setup, both saved-keyless Cloud cases and recovery cannot overlap within one Swift
Testing process. Set `WENDY_E2E_AUTH_MODES_LIVE=true`, select the exact
`--device-address`, and provide `WENDY_E2E_AUTH_MODES_TARGET` pointing to a private
JSON file with these fields:

```json
{
  "device": "dedicated-board.local",
  "resource": "ci-owned-board",
  "holder": "the exact reservation holder",
  "recoveryDirectory": "/private/tmp/auth-modes-dedicated-run",
  "cloudGRPC": "api.dev.example:443",
  "tenant": "the-disposable-tenant-uuid",
  "mdnsInstance": "dedicated-board",
  "disposableEnrollment": true
}
```

`RANCH_ORGANIZATION_ROOT/reservations/<resource>` must already contain that exact
holder and a future ISO8601 `until` time. The suite never creates/releases a
reservation. The runner owns that booking through final cleanup and must not run
another process against the same target concurrently. A fresh device journey
requires an independently observed Open baseline. Reconcile a prior journal
under the same booking before preparing new keys; recovery failure retains the
journal and credentials and fails the test. No fault injection, privileged shell,
updater, recovery boot or automated flash is performed.

`WENDY_E2E_AUTH_MODES_DISCOVERY=true` enables target-only TXT observations on
macOS with `dns-sd`. Provide `mdnsInstance` for this exact board and use its
`.local` hostname as the explicit device. The observer resolves only that named
`_wendyos._udp` instance, checks the returned hostname and expected `access` TXT,
and terminates every DNS-SD process. It never browses or probes other boards.
The device case observes Open/Closed; with the Cloud opt-in, the corresponding
case also observes Enrolled and returned Closed. Unsupported discovery platforms
are skipped. The coverage map separates implemented-but-unrun live assertions
from sub-assertions with no executable check.

Device cases use the existing immutable prebuilt HTTP fixture, separate owner,
recovery-owner, operator, viewer and stranger keys, independent HTTP observations
and exact app cleanup. Keyless transitions use a bounded read-only app readiness barrier beyond the
Agent's scheduled restart budget. Only classified transient transport failures
may retry; mutations and authorization/setup failures are never retried.
The prebuilt Compose journey does not claim physical
registry image-push coverage. `WENDY_E2E_AUTH_MODES_REBOOT=true` additionally opts
into the normal operator reboot test. It reads only
`/proc/sys/kernel/random/boot_id` through the authorized owner's normal
`device shell -- cat` command on the selected target, requiring two valid,
different boot UUIDs. No second SSH login is needed.

Cloud adds `WENDY_E2E_AUTH_MODES_CLOUD=true` and the runner's dedicated
`--cli-auth-config /private/path/auth.json`. Exactly one operator context must
match the configured TLS endpoint and tenant. The scenario copies that fixture
under runner isolation; no ambient login is used. The recovery directory holds
only the test's generated keys and copied dedicated session until restoration.
Each enrollment uses a fresh name and records its returned UUID; no legacy asset
ID is required. Before deletion, the public read-only
`device unenroll --check --asset-id <exact UUID>` independently verifies the
installed device principal, leaf, tenant, endpoint, Cloud binding and guarded
reset capability. Cleanup refuses a changed UUID, never deletes by name alone,
and uses the normal Cloud-owned signed removal path. Secret config values and
private keys are never printed. Run with the same runner entry point and filter
`AuthModesLiveAcceptance`; Cloud and reboot cases remain skipped unless their
additional switches are set.

USB proof is externally provisioned. Select exactly one
`WENDY_E2E_AUTH_MODES_USB_ROUTE` (`orin`, `thor`, `dragonwing`), provide
`--agent-address` for that board, and set `WENDY_E2E_AUTH_MODES_USB_RECEIPT` to a
private JSON receipt with `route`, `device`, `imageSHA256`,
`installerExitStatus`, `firstBootID`, `fingerprint`,
`installerConfigDirectory` and the actual remote `accessFile` path. A person or
separately authorized provisioner performs `install --close`; these tests only
verify the resulting first boot, seeded owner, file mode and second-key refusal.
Unselected routes are skipped. The receipt is evidence of external provisioning,
not proof that this suite performed a flash or proved post-install reboot
persistence. Outstanding assertions remain listed in the coverage map.
