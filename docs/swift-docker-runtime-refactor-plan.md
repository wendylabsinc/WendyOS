# Swift Docker runtime refactor plan

## Goal

Make the Swift Docker path match the Go/Linux behavior as closely as possible:

- container survives client disconnect
- `WendyApp` does not store a `Task`
- structured concurrency owns attach and log sessions, not container lifetime
- app state updates independently from the streaming RPC

## Summary

The current Swift Docker path models a running app as an attached `docker run`
subprocess and stores its `Task` on `WendyApp`. That ties container lifetime to
an RPC-owned session and makes disconnect semantics differ from the Go/Linux
implementation.

The target design is:

- Docker owns container lifetime
- Swift owns only attach and log session lifetime
- `startContainer` launches containers detached
- streaming APIs observe or attach to an already-running container
- a monitor or reconciler updates app state when containers exit on their own

## Recommended defaults for the first implementation

To minimize risk while changing ownership semantics, the first implementation
should intentionally choose the simplest possible session model.

### Output streaming

Use `docker logs -f <containerName>` first for `StartContainer` output
streaming.

Why:

- it is clearly observational rather than ownership-coupled
- cancellation ends only the log session
- it is simpler than interactive attach semantics

`docker attach --no-stdin --sig-proxy=false` remains a good later upgrade if the
agent needs closer parity with attached stdout and stderr behavior.

### PID reporting

Use `nil` for Docker-backed app PIDs in the first implementation.

Why:

- it avoids storing the Docker CLI PID, which is misleading
- it keeps the first refactor independent from `docker inspect`
- a real container PID can be added later if it proves useful

## Delivery strategy

Preferred delivery: one PR.

This can be delivered in a single PR, but it should still be built as a staged
sequence of commits so review stays understandable.

Recommended commit order inside one PR:

1. remove `dockerRunTask` plumbing from `WendyApp` and `ContainerService`
2. switch Docker launch to detached mode
3. add structured `docker logs -f` streaming for `StartContainer`
4. make stop and delete Docker-driven and add lightweight reconciliation
5. add the Docker monitor and final test coverage

The PR can stay single-shot as long as each commit preserves the intended
ownership model and the final diff includes focused tests for disconnect
semantics.

## Phase 1: Remove Docker task state from `WendyApp`

### File

- `swift/WendyAgentCore/Sources/WendyAgent/WendyApp.swift`

### Changes

- remove `dockerRunTask: Task<Void, Never>?`
- keep `process` for the native Darwin path for now
- keep `launchToken` only if still useful for native stale-callback protection

### Follow-up

Update `ContainerService` helpers so they stop reading or writing Docker task
state:

- `prepareAppForLaunch`
- `cancelAppLaunch`
- `markAppRunning`
- `markAppStopped`
- `stopTrackedAppIfRunning`

## Phase 2: Launch Docker containers detached

### File

- `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`

### New start flow

Refactor the Docker branch of `startContainer` to:

1. validate the app and Docker availability
2. best-effort remove any stale container with the same name
3. build `docker run` arguments as today
4. launch with `docker run -d --rm --name ...`
5. mark the app running with `pid: nil` in the first cut
6. return a stream that only observes output
7. optionally add container inspection later if runtime details are needed

### Notes

- `startContainer` should no longer create or return a background `Task`
- `markAppRunning` should no longer accept `dockerRunTask`
- the stored PID should no longer be the Docker CLI PID

### PID policy

First implementation:

- store `nil` for Docker-backed apps

Possible later enhancement:

- inspect and store the real container PID if that proves stable and useful

## Phase 3: Replace attached `docker run` with structured attach or log streaming

### Remove

Delete the attached-run bridge from `ContainerService.swift`:

- `AttachedDockerRun`
- `launchAttachedDockerRun(...)`

### Add

Introduce a helper that only streams output from an already-running container,
for example:

- `streamDockerContainerOutput(containerName:appName:broadcaster:writer:)`

### Streaming options

#### First implementation

Use `docker logs -f <container>` for `StartContainer` output streaming.

Pros:

- simplest observational model
- clearly decoupled from runtime ownership
- cancellation ends the log session without implying container shutdown

Cons:

- may lose stdout and stderr separation
- less exact than a true attach session

#### Later enhancement

Use `docker attach --no-stdin --sig-proxy=false <container>` if the agent needs
closer parity with attached stdout and stderr behavior.

Pros:

- closer to current attached behavior
- more likely to preserve stdout and stderr separation

Cons:

- more session-like than plain log streaming

### Structured concurrency shape

Inside the returned `StreamingServerResponse` producer:

- send the `.started` response
- run the attach or log subprocess with `swift-subprocess`
- consume its async output sequences directly
- forward chunks to the gRPC writer
- forward log text to telemetry broadcasting
- on cancellation, stop only the attach or log subprocess
- do not stop the Docker container

## Phase 4: Make stop and delete Docker-driven

### File

- `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`

### Stop flow

For Docker-backed apps, `stopTrackedAppIfRunning` should:

1. call `docker stop --time 10 <containerName>`
2. fall back to `docker kill <containerName>` if needed
3. let monitoring or reconciliation observe the final stopped state, or mark it
   stopped after a successful stop

### Delete flow

`deleteContainer` should:

1. stop the container if needed
2. best-effort remove it with `docker rm -f <containerName>`
3. remove the app record
4. publish app updates

### Important

Do not wait on or cancel a stored Docker `Task`. Docker becomes the owner of
runtime lifetime.

## Phase 5: Add a Docker runtime monitor

### New file

- `swift/WendyAgentCore/Sources/WendyAgent/Services/DockerContainerMonitor.swift`

### Responsibility

Watch managed Docker containers and notify `ContainerService` when they start,
stop, die, or are removed.

### Preferred mechanism

Run a long-lived `docker events` subprocess with filters for managed Wendy
containers, for example using labels already applied today:

- `wendy.managed=true`
- `wendy.app-name=<appName>`

Watch for events such as:

- `start`
- `die`
- `stop`
- `destroy`

### Service integration

`ContainerService` should:

- start the monitor during service startup or readiness
- stop the monitor during shutdown
- receive callbacks back on actor isolation

### Suggested actor methods

- `handleDockerContainerStarted(appName: String, pid: Int32?)`
- `handleDockerContainerStopped(appName: String)`
- `handleDockerContainerRemoved(appName: String)`

### Why this matters

Detached containers can exit after the client disconnects. Without a monitor,
Swift will not update app state until a later RPC happens.

## Phase 6: Add reconciliation as a fallback

### File

- `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`

### Add helper methods

Introduce helpers such as:

- `refreshDockerAppStateIfNeeded(id:)`
- `reconcileAllDockerApps()`

### Use them from

- startup
- list operations
- error recovery paths in stop or delete
- monitor restart or recovery paths

### Reconciliation behavior

For each registered Docker app:

- inspect whether the named container exists
- if running, ensure the app is marked running
- if absent or exited, ensure the app is marked stopped

This protects the service if Docker events are missed or the daemon restarts.

## Phase 7: Align future `AttachContainer` behavior

### Goal

Make `AttachContainer` a session-only API that does not own runtime lifetime.

### Shape

Implement it as a structured subprocess session over:

- `docker attach <containerName>`

with:

- stdin forwarded from the client stream into the subprocess input writer
- stdout and stderr forwarded back to the client
- cancellation closing only the attach session

### Disconnect semantics

If the client disconnects:

- close stdin
- end the attach subprocess
- leave the container running

As in Go, the app may still exit on its own if stdin EOF is meaningful to it,
but the agent should not force-stop it.

## File-by-file checklist

### `swift/WendyAgentCore/Sources/WendyAgent/WendyApp.swift`

- remove `dockerRunTask`
- keep persistence behavior unchanged

### `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`

Refactor:

- `prepareAppForLaunch`
- `cancelAppLaunch`
- `markAppRunning`
- `markAppStopped`
- `stopTrackedAppIfRunning`
- Docker branch of `startContainer`
- `deleteContainer` as needed

Remove:

- `AttachedDockerRun`
- `launchAttachedDockerRun(...)`

Add:

- detached Docker start helper
- inspect or reconcile helper
- attach or log streaming helper
- monitor callback handlers

### `swift/WendyAgentCore/Sources/WendyAgent/Services/DockerContainerMonitor.swift`

Add a new monitor type for `docker events` integration.

### Tests

Update or extend:

- `swift/WendyAgentCore/Tests/WendyAgentTests/ContainerServiceTests.swift`

Add coverage for:

- no Docker task handle stored in app state
- stream disconnect does not stop the container
- stop and delete work without stored task state
- spontaneous container exit updates app state through monitoring or
  reconciliation

## Suggested implementation order

Even if this lands as a single PR, implement it in this order:

1. remove `dockerRunTask` plumbing
2. switch Docker launch to detached mode
3. add structured `docker logs -f` streaming and use `pid: nil`
4. make stop and delete Docker-driven
5. add lightweight reconciliation on startup and key RPC paths
6. add the Docker monitor
7. tighten tests and decide whether a later `docker attach` upgrade is still
   needed

## Open decisions

### Future upgrade from `docker logs -f` to `docker attach`

Default for the first implementation:

- `StartContainer` uses `docker logs -f`

Later decision:

- evaluate whether `docker attach --no-stdin --sig-proxy=false` is worth the
  extra complexity for better stdout and stderr parity

### Future Docker PID reporting

Default for the first implementation:

- Docker-backed apps expose `nil` PID

Later decision:

- evaluate whether the actual container PID from `docker inspect` is worth
  surfacing

### Monitor lifecycle

Decide exactly where `DockerContainerMonitor` starts and stops in the service
lifecycle.

### `launchToken` scope

Once Docker is detached and monitor-driven, `launchToken` may remain necessary
for native apps while becoming less important for container apps.

## Recommended end state

### Docker-backed apps

- Docker owns runtime lifetime
- Swift RPCs own only attach and log session lifetime
- app state comes from monitoring and reconciliation
- `WendyApp` stores no Docker task handle

### Native apps

- keep the current `Foundation.Process` path for now
- consider a separate cleanup later if native runtime handles should also move
  out of `WendyApp`
