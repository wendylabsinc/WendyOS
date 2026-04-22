# kb.address-feedback plan

Address PR #478 feedback items 2 and 3 in small stages so we reduce risk and keep each change easy to review.

Feedback item 4 (`BonjourAdvertiser` concurrency cleanup) was moved to its own worktree/branch `kb.no-dispatch-queues` because it is orthogonal to the Docker subprocess work and should ship as a separate PR.

## Scope

1. `DockerCLI.run(...)` should not buffer all stdout until process exit.
2. Docker process management should move off `Foundation.Process` and onto `swift-subprocess`.

## Stage 1 — Migrate Docker command execution to `swift-subprocess` (done)

Goal: address the broader process-management feedback first, and solve the stdout buffering issue as part of the new subprocess design instead of building a better `Foundation.Process` implementation that we would immediately replace.

Status: landed on this branch. Attached Docker runs still use `Foundation.Process` and are addressed in Stage 2.

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

## Stage 2 — Fold `DockerCLI` into `ContainerService`

Goal: replace `DockerCLI` with domain-shaped methods that live directly on `ContainerService` (repository pattern), and migrate attached Docker runs onto `swift-subprocess` in the same move. The attached-run buffering fix from the earlier Stage 2 plan falls out for free: the attached call site becomes a `Subprocess.run` closure inside `ContainerService.startContainer` with no wrapper types.

### Motivation
- `DockerCLI` today is a generic CLI abstraction (`RunOption`, `RmOption`, `run(arguments:)`, `ps(label:)`) that every caller composes into business behavior.
- Every Docker operation we actually perform is already a named business operation on `ContainerService` (`createContainer`, `startContainer`, `stopContainer`, `deleteContainer`). The generic layer has exactly one consumer and adds indirection without isolating anything domain-worthy.
- The repository/SQL pattern fits better: each `ContainerService` method is the "query", parameters are in our own domain, and the method owns the concrete `docker` invocation it needs.
- `DockerContainerBackend` was already folded into `ContainerService` earlier on this branch; this stage completes the same direction of travel by folding `DockerCLI` itself.

### Changes
- Delete `DockerCLI`, `DockerCLI.RunOption`, `DockerCLI.RmOption`, and the generic `DockerError` shape.
- Inline Docker invocations into the relevant `ContainerService` methods:
  - `createContainer` (Linux path) — `docker pull`.
  - `startContainer` (Linux path) — `docker rm -f` + `docker run` attached via `Subprocess.run` streaming closure.
  - `stopTrackedAppIfRunning` (Linux path) — `docker stop`.
  - `deleteContainer` (Linux path) — `docker rm -f`.
- Keep the entitlement → `docker run` flag translation inside `ContainerService` (already moved there in the `DockerContainerBackend` fold); simplify it further as needed once the call site is local.
- Add a private `runDocker(_:timeout:)` helper on `ContainerService` for bounded `Subprocess` invocation and exit-status → `RPCError` translation. Never exposed.
- Move the Docker-availability probe and registry-ensure dance onto a single `ContainerService.ensureReady()` method called once at startup; drop the `dockerAvailable: Bool` init parameter.
- Inject `dockerExecutablePath: String = "docker"` on `ContainerService.init` so existing fake-shell-script tests keep working verbatim.

### Design targets
- Every Docker invocation has exactly one call site, in the method that owns that business operation.
- Attached runs consume `Subprocess.run`'s stdout/stderr streams directly inside a closure — no wrapper types, no bridge actor, no returned handle.
- `ContainerService` methods read as "what the agent does when asked to X", not as "assemble Docker flags and invoke a CLI".
- Do not over-translate parameters into domain types; Docker-shaped primitives (e.g. `timeout: Duration` on stop) are fine where they are already the right abstraction.

### Acceptance criteria
- Resolves PR #478 feedback item 2 (`swift-subprocess` for all process management) in full.
- Attached Docker runs stream stdout/stderr without unbounded buffering.
- Docker availability probe and registry setup still happen exactly once at agent startup.
- No regressions in the native darwin path.

### Validation
- Existing `ContainerService` tests continue to pass.
- `DockerCLITests` folds into tests that target `ContainerService` directly via the injected `dockerExecutablePath:` (fake shell scripts).
- Manual smoke tests:
  - agent startup with docker present → registry container is running.
  - agent startup without docker → Linux-container RPCs fail cleanly with `failedPrecondition`.
  - Linux container create/start/stop/delete cycle end-to-end.
  - native darwin app launch unaffected.

### Suggested commit split
1. Fold `DockerCLI` into `ContainerService` for the one-shot operations (`pull`, `stop`, `rm`, `ensureReady`); attached path still uses `Foundation.Process` at this point.
2. Migrate the attached Docker run onto a `Subprocess.run` streaming closure inside `ContainerService.startContainer`; delete any remaining `Foundation.Process` use on the Docker path.

Each commit is independently reviewable.

## Suggested order of implementation

1. Stage 1 — Docker one-shot commands on `swift-subprocess`. (done)
2. Stage 2 — fold `DockerCLI` into `ContainerService` and migrate attached runs.

## Deliverables

- One plan file (this file).
- Roughly 3 focused commits across the two stages:
  1. (Stage 1, done) `swift-subprocess` migration for one-shot Docker command execution.
  2. (Stage 2) fold `DockerCLI` into `ContainerService` for one-shot operations.
  3. (Stage 2) migrate attached Docker runs onto `Subprocess.run` inside `ContainerService.startContainer`.

The Bonjour concurrency cleanup lives in the `kb.no-dispatch-queues` worktree as its own plan.
