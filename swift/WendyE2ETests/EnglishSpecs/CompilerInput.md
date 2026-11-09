# Compile four English cases into stored Swift E2E tests

The only behavior inputs are the four neighboring files: `cache-clear.md`,
`entitlement-add.md`, `entitlement-invalid.md` and `device-default.md`. Do not
read original Swift E2E test implementations or product implementation code.
Do not run another model, agent, CLI or tests while compiling. Output Swift
source files only, one file and one suite per English case. Runtime execution
must never call a model. Use Foundation, Testing and WendyE2ETesting imports.

Place output in `Tests/WendyE2ETests/GeneratedEnglishSpecs/` with filenames
`EnglishCacheClearTests.swift`, `EnglishEntitlementAddTests.swift`,
`EnglishEntitlementInvalidTests.swift`, and `EnglishDeviceDefaultTests.swift`.
Use unique sentence-style suite and test names prefixed with `English spec`.
Each file contains one runnable test. Include the case's setup, action,
expectations and cleanup in a documentation comment immediately before `@Test`.
Do not disable tests, swallow errors, loosen assertions or invent APIs.

## Harness API available to generated tests

These APIs already exist in the test target. This is an API description, not an
implementation of any of the four cases.

```swift
final class CLIAndAgentScenario: Sendable {
    typealias Hook = @Sendable (WendyE2ESession, WendyE2ESession) async throws -> Void
    init(before: Hook? = nil, after: Hook? = nil)
    func run<Result>(
        authenticated: Bool = true,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (WendyE2ESession, WendyE2ESession) async throws -> Result
    ) async throws -> Result
}

// WendyE2ESession is an actor. This property is nonisolated.
var wendyCacheDirectory: String { get }

// These are instance methods on WendyE2ESession.
func sh(_ command: String) async throws
func sh<Result>(
    _ command: String,
    _ body: (WendyE2EShellResult) async throws -> Result
) async throws -> Result
func sh(posix: String, power: String) async throws
func sh<Result>(
    posix: String, power: String,
    _ body: (WendyE2EShellResult) async throws -> Result
) async throws -> Result

// WendyE2EShellResult properties:
var stdout: String { get }
var stderr: String { get }
var readableStderr: String { get }
var status: WendyE2EShellStatus { get }
// status.isSuccess and status.isFailure are Bool.
```

Create the scenario inside each test. Call `run(authenticated: false)` and use
the first callback parameter as the CLI session. The second parameter is unused;
creating this session does not launch or contact an agent. The harness supplies
an isolated HOME, TMPDIR, working directory, the pinned real CLI on PATH, and
`WENDY_ANALYTICS=false`. All fixture I/O must use the CLI session so it remains
in the right sandbox. Never use host-global FileManager paths for fixture I/O.

The no-callback `sh` overload throws on nonzero exit. The callback overload
lets you inspect expected failure statuses and does not automatically assert
success. Use `#expect` for checks and `try #require` when later checks depend on
a decoded value. JSONSerialization or a small Decodable type in the generated
file may be used; no other test file's decoder is available as an input.
`readableStderr` unwraps a JSON error envelope for text assertions.

The `after` hook runs even when setup or the body throws, and its errors are
propagated. Use it for case-owned fixture cleanup. Do not use `try?`, `|| true`
or empty catch blocks to hide a failed cleanup. Do not remove the sandbox root
or recordings. POSIX and PowerShell command variants are supported; retain
both for fixture operations that use platform-specific syntax. Local validation
is macOS, and unexecuted platforms must not be claimed as validated.

The stored output must be ready for the repository's `swift format` formatting.
Formatting is part of generation; output after that step is stored without hand
edits. Any compilation or assertion correction requires a recorded prompt change
and fresh generation, never editing a generated test directly.
