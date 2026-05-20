# Plan: build the Swift E2E behavioral spec

## Goal

Turn `swift/WendyE2ETests` into the executable behavioral specification for
the Wendy CLI, agent, device, cloud, and project workflows.

The suite should not grow by casually encoding current behavior. For each command
area, first write and review disabled spec stubs that describe the desired
user-facing behavior. Once the stubs read like a complete product/API spec, make
them executable one by one.

## Guiding principle

The E2E suite is the product spec.

Every spec should describe desired behavior from the user's point of view. When
the current product is incomplete, the spec should make that gap visible instead
of treating today's limitation as correct.

If the current Mac Agent returns
`RPCError(code: .unimplemented, message: "... currently not supported on macOS.")`
for behavior that should eventually work, do not write a passing spec that
asserts `.unimplemented`. Assert the desired behavior and let the record expose
the implementation gap. Only assert unsupported behavior as success when that is
the intended user-facing contract.

## Workflow

Work one bounded command area at a time.

1. Pick one command area.
2. Build a behavioral matrix for that area.
3. Add disabled Swift Testing spec stubs only.
4. Review the stubs as product/API documentation.
5. Revise until the stubs are agreed as a good-enough spec.
6. Implement the specs incrementally.
7. Inspect command records and refine fixtures/assertions as needed.

Do not skip the stub review step. Disabled stubs are the durable handoff between
sessions and between product/design/testing decisions.

## Spec stub style

Use disabled tests so unimplemented specs do not falsely pass:

```swift
@Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
func `creates a minimal Swift WendyOS project non-interactively`() async throws {
    // Given: an empty temporary directory
    // When: `wendy init` is run with app id, target, language, no extra entitlements, and no git init
    // Then:
    // - exits successfully
    // - writes wendy.json
    // - writes Package.swift
    // - emits concise success guidance on stdout
    // - emits no stderr diagnostics
}
```

A good stub:

- reads like product documentation
- names one user-visible behavior
- states setup, action, and expected outcomes
- identifies filesystem/config mutations and non-mutations
- avoids incidental current wording unless the wording is itself the contract
- calls out platform requirements when relevant

## Behavioral matrix

For each command area, consider:

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
- platform-specific behavior
- agent/device/cloud requirements

For human-readable output, prefer semantic anchors unless exact text is part of
the contract. For JSON and config files, prefer exact structure and meaningful
fields.

## Machine/session model

`Machine` is static metadata and can be used in `.enabled(if:)` traits.
`Session` is the runtime command executor.

Known machines:

```swift
Machine.current  // test runner, tagged `.runner`
Machine.cli
Machine.agent
```

Known OS values:

```swift
.macOS
.linux
.windows
.wendyOS
```

Use `Session.begin(for:)`, `session.end()`, or `Session.with(...)` to execute
commands. Use `session.sh(...)` for immediate shell execution and
`session.command(...).poll(...).run()` for configured command execution.

## Platform policy

The E2E package must run on Linux as well as macOS. Ubuntu LTS is the first Linux
runner target. WendyOS on Jetson/Raspberry Pi comes after the Linux runner path
is stable.

If a spec needs a local macOS app-backed agent, gate it to macOS for now. Do not
let macOS-only lifecycle assumptions make the Linux suite fail.

Prefer static machine metadata for gating:

```swift
@Test(.enabled(if: Machine.cli.os == .linux))
```

Use compile-time `#if os(...)` only when code cannot compile cross-platform.

## Fixtures and infrastructure

Add helpers as needed, but keep them in service of the spec:

- temporary `HOME` and config directories
- platform-aware cache/config paths
- temporary Wendy projects and generated `wendy.json` files
- local fake Wendy Cloud services where cloud behavior must be deterministic
- local fake or simulated WendyOS agents where device behavior must be deterministic
- real-device gates for behavior that cannot be simulated faithfully
- local Mac Agent lifecycle helpers for commands that intentionally exercise macOS agent behavior
- project builders and sample apps for build/run/deploy specs
- reusable JSON and output assertion helpers

Do not avoid an important behavior because the fixture is involved. Specify it,
then make it verifiable.

## Handling implementation gaps

For desired behavior that is not implemented yet:

- Keep the desired assertions in the spec.
- Use disabled stubs until the behavior and fixture strategy are agreed.
- Once implemented as an executable spec, failures should be useful evidence of
  missing implementation.
- If the default suite must stay green temporarily, use explicit gating or known
  issue mechanisms rather than weakening the desired assertion.

## Assertions

Prefer strong, user-facing assertions:

- exact output for stable short messages
- parsed JSON shape and values for `--json`
- non-zero exit plus clear diagnostics for intended failure cases
- real filesystem/config side effects for commands that write state
- observable device/agent/cloud effects for integration scenarios
- readable progress/log output for long-running commands

Avoid broad assertions that make different behaviors equivalent, such as:

```swift
#error contains domain-specific text || error contains "Could not connect"
```

Those are acceptable only for rough smoke coverage, not for a behavioral spec.

## Implementation order

Recommended order, one command area at a time:

1. Make `TestE2E.sh` work out of the box on Ubuntu LTS.
2. `wendy device version` spec stubs, review, then implementation. Start with the CLI-to-agent `GetAgentVersion` interaction, using the local macOS app-backed agent behind macOS gates until a deterministic fake/simulated agent fixture exists for Linux.
3. top-level CLI, help, completion, info, analytics, json
4. `wendy init` spec stubs, review, then implementation
5. project configuration
6. project entitlements
7. build and run
8. cache and OS cache
9. discovery and device selection/default-device behavior
10. device dashboard, logs, telemetry, update
11. device apps and volumes
12. device hardware, camera, audio, Bluetooth, WiFi
13. OS image download, install, list-drives, update
14. auth and cloud-backed flows
15. tour and utilities

This order is for focus and reviewability only. Later areas are not optional.

## Running tests

Use an explicit records directory while iterating:

```bash
cd swift/WendyE2ETests
WENDY_AGENT_E2E_TEST_RECORDS_DIR="$PWD/.build/e2e-test-records.current" swift test --filter WendyE2ETests
```

For focused work:

```bash
swift test --filter '<suite-or-test-fragment>'
```

Inspect the Markdown command records after each command family. Records are part
of the feedback loop: they should show whether the implementation matches the
spec or where it falls short.

## Acceptance criteria

This work is successful when:

1. The E2E harness runs out of the box on macOS and Ubuntu LTS.
2. Each command area has reviewed disabled spec stubs before implementation.
3. Implemented specs assert desired user-facing behavior, not accidental current behavior.
4. No test passes only because its body is empty.
5. Current `.unimplemented` agent responses are visible as gaps unless they are the intentional product contract.
6. Fixtures, fakes, gates, and helpers exist wherever needed to make the spec verifiable.
7. Command records provide clear evidence for both passing behavior and missing implementation.
