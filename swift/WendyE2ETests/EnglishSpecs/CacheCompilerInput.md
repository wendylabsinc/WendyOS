# Compile only the maintained cache-clear English case

The behavior input is the neighboring `cache-clear.md`. Do not read original
Swift tests, previous generated output or product implementation code. Do not
call tools or run tests while compiling. Return only one complete Swift file,
starting with the exact marker `// FILE: EnglishCacheClearTests.swift`, with no
Markdown fences. This output replaces only
`Tests/WendyE2ETests/GeneratedEnglishSpecs/EnglishCacheClearTests.swift`.

Use Foundation, Testing and WendyE2ETesting imports. Define one unique suite and
one runnable test, both with sentence-style display names prefixed `English spec`.
Include setup, action, expectations and cleanup in a documentation comment
immediately before `@Test`. Preserve all baseline assertions as well as the added
POSIX requirements. Do not skip the whole test on Windows. Do not invoke a model
at runtime. Do not swallow errors, loosen expectations or invent APIs.

## Existing harness APIs

`CLIAndAgentScenario` is available inside the test target. Initialize it inside
the test with optional `before` and `after` hooks. Each hook has the signature
`@Sendable (WendyE2ESession, WendyE2ESession) async throws -> Void`. Its `after`
hook runs even after setup or body failure and propagates cleanup errors.

Run it as `try await scenario.run(authenticated: false) { cli, _ in ... }`.
The first argument is the CLI session; the unused second session launches no
agent. The harness supplies isolated HOME, TMPDIR and working directory, the real
pinned CLI on PATH, command recording and `WENDY_ANALYTICS=false`. All fixture
I/O must use the CLI session. Preserve the run directory and recordings.

`WendyE2ESession` is an actor with these nonisolated properties:

- `wendyCacheDirectory: String`: the correct platform-specific cache path.
- `machine.os`: one of `.macOS`, `.linux`, `.wendyOS`, `.windows`.

Its command methods are:

```swift
func sh(_ command: String) async throws
func sh<Result>(
    _ command: String,
    body: (WendyE2EShellResult) async throws -> Result
) async throws -> Result
func sh(posix: String, power: String) async throws
func sh<Result>(
    posix: String, power: String,
    body: (WendyE2EShellResult) async throws -> Result
) async throws -> Result
```

Use trailing closures for result callbacks. The no-callback overload throws on
nonzero status. The callback overload does not assert success automatically:
assert `result.status.isSuccess` or `isFailure` explicitly. Result `stdout` and
`stderr` are strings; `readableStderr` unwraps JSON error envelopes. `#expect`
records failures, and `try #require` enforces prerequisites.

The variant overload chooses POSIX for macOS/Linux/WendyOS and PowerShell for
Windows. Use POSIX `stat -f %Lp` on macOS and `stat -c %a` on Linux/WendyOS to
check permission modes. Quote dynamic paths. Multi-command fixture scripts must
fail on any failed setup, verification or cleanup command. Use no `try?`,
`|| true` or empty catch blocks. Never touch the real user home or cache.

Output should be ready for `swift format`. Mechanical formatting is part of
generation; no generated source may be hand-edited after that step. Compilation
or assertion failures require recorded prompt corrections and regeneration.
