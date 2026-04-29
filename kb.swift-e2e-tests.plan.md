# Plan: minimal `Machine`-based Swift E2E tests

## Goal

Implement the mac E2E test setup as a tiny, script-like Swift API that reads roughly like this:

```swift
let runner = Machine.ssh("user@host:~/blah/blub")
let cli = Machine.ssh("user@host:~/blah/blub")
let agent = Machine.ssh("user@host:~/blah/blub")

try await runner.run("cd swift && make build-dev")
try await runner.run("cd go && make build")

try await runner.push("go/bin/wendy", to: cli)
try await runner.push("fixtures/HelloMac", to: cli)
try await runner.push("swift/Build/WendyAgentMac.app", to: agent)

try await agent.run("open WendyAgentMac.app")
```

This should stay deliberately small and direct.

## Non-goals

Do **not** build any of this in the first pass:

- topology frameworks
- generalized host/config models
- artifact managers
- reusable node graphs
- SSH libraries
- broad fixture systems
- multi-agent or multi-CLI orchestration

## Minimal design

### `Machine`

Use one small type to represent a place where commands can run.

Suggested shape:

```swift
class Machine {
    static func local(_ path: String) -> Machine
    static func ssh(_ spec: String) -> Machine

    func run(_ command: String) async throws
    func push(_ sourcePath: String, to destination: Machine, at destinationPath: String? = nil) async throws
}
```

### SSH spec

`Machine.ssh(...)` should accept a compact scp-style string:

```swift
user@host:~/repo
```

That gives us exactly two things:

- SSH target: `user@host`
- checkout/base directory: `~/repo`

No separate config object is needed at first.

### `run`

`run` should:

- execute locally with `Process` when the machine is local
- execute remotely with `ssh` when the machine is remote
- fail clearly on non-zero exit
- stream or capture enough output to debug failures

Keep it shell-string based. No command DSL.

### `push`

`push` should:

- copy a file or directory from one machine to another
- use system tools only (`scp -r` is enough for v1)
- treat the input as a path relative to the source machine's base directory
- copy into the destination machine's base directory, preserving the trailing name

Examples:

- `go/bin/wendy` -> `<dest>/go/bin/wendy` or `<dest>/wendy`
- `fixtures/HelloMac` -> destination fixture dir
- `swift/Build/WendyAgentMac.app` -> destination app bundle path

Pick one simple destination rule and keep it consistent. Prefer preserving the relative path if it keeps commands obvious.

## Test shape

Start with a single smoke test that does only this:

1. build the Swift app on `runner`
2. build the Go CLI on `runner`
3. copy the CLI binary to `cli`
4. copy one tiny fixture app to `cli`
5. copy `WendyAgentMac.app` to `agent`
6. launch the app on `agent`
7. run one CLI command against the agent
8. assert success

That is enough for the first pass.

## Placement

Keep the code in the smallest possible Swift test target.

Preferred layout:

- one new Swift test target/package for E2E tests
- one helper file for `Machine`
- one test file for the smoke test

Do not split into many helper modules yet.

## Configuration

Keep configuration minimal.

First pass options, in order of preference:

1. hard-code the three `Machine` values inside the test while iterating
2. then, if needed, move just those strings to environment variables

If env vars are needed, keep it to a tiny set such as:

- `E2E_RUNNER`
- `E2E_CLI`
- `E2E_AGENT`

where each value is either:

- `local:/path/to/repo`
- `user@host:/path/to/repo`

No larger config surface yet.

## Implementation steps

1. Add the smallest Swift test target/package for E2E tests.
2. Implement `Machine.local` and `Machine.ssh`.
3. Implement `run(_:)` using `Process` + `ssh`.
4. Implement `push(_:to:)` using `scp -r`.
5. Write one smoke test that follows the exact build/copy/open flow above.
6. Add minimal cleanup for the launched app if needed.
7. Verify the test reads close to the sketch and remove any extra abstraction that appeared along the way.

## Acceptance criteria

This plan is successful when:

1. the test code looks close to the sketch above
2. there is a single small `Machine` helper instead of a framework of helper types
3. builds happen on `runner`
4. artifacts are pushed to `cli` and `agent`
5. the agent app is launched remotely with `open`
6. one real CLI-to-agent smoke test passes

## Guiding rule

If a design choice makes the code look less like a short shell script written in Swift, it is probably too much for this phase.
