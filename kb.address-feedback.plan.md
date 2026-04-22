# kb.address-feedback plan

Address PR #478 feedback items 2 and 3 in small stages so we reduce risk and keep each change easy to review.

Feedback item 4 (`BonjourAdvertiser` concurrency cleanup) was moved to its own worktree/branch `kb.no-dispatch-queues` because it is orthogonal to the Docker subprocess work and should ship as a separate PR.

## Scope

1. `DockerCLI.run(...)` should not buffer all stdout until process exit.
2. Docker process management should move off `Foundation.Process` and onto `swift-subprocess`.

## Stage 1 — Migrate Docker command execution to `swift-subprocess`

Goal: address the broader process-management feedback first, and solve the stdout buffering issue as part of the new subprocess design instead of building a better `Foundation.Process` implementation that we would immediately replace.

### Changes
- Update `swift/WendyAgentCore/Package.swift` to add `swift-subprocess`.
- Refactor `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerCLI.swift`.
- Replace the current one-shot `Foundation.Process`-based `run(arguments:timeout:)` implementation with a `swift-subprocess`-backed implementation.
- Preserve the external `DockerCLI` API shape where practical so callers do not need broad changes.

### Design targets
- Make streamed or incremental stdout/stderr handling part of the new design from the start.
- Avoid the current `readDataToEndOfFile()`-on-termination pattern.
- Ensure long-running or chatty commands do not require unbounded in-memory buffering.
- Keep timeout and cancellation behavior explicit and robust.
- Make failure reporting clear without reintroducing Foundation-specific lifecycle edge cases.

### Acceptance criteria
- The migration resolves both review items:
  - use `swift-subprocess`
  - stop buffering all stdout until process exit
- One-shot commands such as `checkAvailable`, `ensureRegistry`, `pull`, `stop`, `rm`, and `ps` still behave the same from the caller's perspective.
- Timeout/cancellation semantics remain correct.

### Validation
- Build/package resolution succeeds with the new dependency.
- Add/update tests around:
  - large stdout output
  - timeout handling
  - non-zero exit with stderr captured
- Manual smoke tests for representative docker commands.

## Stage 2 — Migrate/clean up attached Docker container execution

Goal: finish the Docker-side refactor by moving attached container launching onto the same subprocess model and confirming output is streamable without retaining unbounded logs in memory.

### Changes
- Refactor:
  - `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerCLI.swift`
  - `swift/WendyAgentCore/Sources/WendyAgent/Docker/DockerContainerBackend.swift`
- Replace `runAttached(...)`'s `Foundation.Process` implementation with a subprocess-backed approach.
- Preserve the higher-level behavior expected by `ContainerService` and related callers.

### Design targets
- Prefer one internal subprocess abstraction so timeout, cancellation, logging, output collection, and attached streaming behavior are implemented consistently.
- Keep attached container stdout/stderr streamable to callers.
- Avoid separate ad-hoc subprocess lifecycle logic for one-shot commands vs attached runs.

### Acceptance criteria
- Attached Docker runs still work end-to-end.
- Output for indefinite or long-lived runs is streamable and does not rely on retaining all bytes in memory.
- Container termination and cleanup behavior remain correct.

### Validation
- Existing Docker flows still work:
  - registry startup
  - `pull`
  - `runAttached`
  - `stop` / `rm` / `ps`
- Confirm attached-run cancellation and teardown still behave correctly after the migration.

## Suggested order of implementation

1. Stage 1 as the main Docker subprocess migration for one-shot commands.
2. Stage 2 to finish the Docker refactor for attached runs and streaming behavior.

## Deliverables

- One plan file (this file).
- Prefer 2 focused commits / PR commits:
  1. `swift-subprocess` migration for Docker command execution, including bounded/streamed output handling
  2. attached Docker execution cleanup on the same subprocess model

The Bonjour concurrency cleanup lives in the `kb.no-dispatch-queues` worktree as its own plan.
