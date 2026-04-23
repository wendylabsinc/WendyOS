# Swift Docker runtime implementation checklist

This checklist turns `docs/swift-docker-runtime-refactor-plan.md` into a
concrete implementation sequence for one PR.

## Scope for the first implementation

Deliver these behavior changes in the first cut:

- Docker-backed containers survive client disconnect
- `WendyApp` no longer stores a Docker `Task`
- Docker-backed `StartContainer` launches detached
- `StartContainer` streams output via structured `docker logs -f`
- Docker-backed apps report `pid: nil`
- stop and delete operate through Docker commands
- lightweight reconciliation keeps app state usable before a full monitor lands

Do not try to add `docker attach` or real container PID reporting in the first
implementation.

## Preferred commit order inside one PR

1. remove Docker task state from `WendyApp` and `ContainerService`
2. switch Docker launch to detached mode
3. add structured `docker logs -f` streaming to `StartContainer`
4. make stop and delete Docker-driven
5. add lightweight reconciliation helpers and tests
6. add the Docker monitor only if still needed in the same PR

## File-by-file implementation checklist

## 1. `swift/WendyAgentCore/Sources/WendyAgent/WendyApp.swift`

### Change

- remove `dockerRunTask: Task<Void, Never>?`

### Verify

- `CodingKeys` remain unchanged
- no other Docker runtime fields are introduced here
- native `process` behavior remains intact

## 2. `swift/WendyAgentCore/Sources/WendyAgent/Services/ContainerService.swift`

## 2.1 Remove Docker task plumbing from app-state helpers

### Update `prepareAppForLaunch(id:launchToken:)`

- keep resetting `info` to stopped
- keep clearing `process`
- remove `dockerRunTask` clearing
- keep writing `launchToken`

### Update `cancelAppLaunch(id:launchToken:)`

- keep clearing `process`
- remove `dockerRunTask` clearing
- keep clearing `launchToken`

### Update `markAppRunning(...)`

Current signature:

- `markAppRunning(id:pid:nativeProcess:dockerRunTask:launchToken:)`

Target first-cut signature:

- `markAppRunning(id:pid:nativeProcess:launchToken:)`

Checklist:

- remove the `dockerRunTask` parameter
- stop writing any Docker task handle into `WendyApp`
- allow `pid` to remain `Int32?` only if needed by the implementation;
  otherwise pass `0` or prefer adjusting the model update to support Docker
  `pid: nil`
- preserve native process handling

### Update `markAppStopped(id:)`

- remove `dockerRunTask` from the early-return comparison
- stop clearing any Docker task state
- keep clearing `process`
- keep clearing `launchToken`

## 2.2 Remove attached-run implementation

### Delete

- `AttachedDockerRun`
- `launchAttachedDockerRun(args:appName:launchToken:)`

### Verify

- no Docker path still depends on `Task.detached`
- no Docker path still uses `AsyncThrowingStream` as a bridge from a detached
  launcher task

## 2.3 Refactor Docker `startContainer`

### Current behavior to replace

The Docker branch currently:

- removes stale container name
- launches attached `docker run`
- stores `spawn.runTask`
- marks app running with the Docker CLI PID
- returns a stream backed by spawned stdout and stderr streams

### Target first-cut behavior

The Docker branch should:

1. remove any stale named container with `docker rm -f`
2. build Docker argv as today
3. add `-d` to `docker run`
4. launch detached via `runDocker(...)`
5. mark app running with `pid: nil`
6. return a stream that uses `swift-subprocess` to run `docker logs -f`

### Checklist

- keep the existing entitlement translation logic
- keep `wendy.managed=true` and `wendy.app-name=<appName>` labels
- keep `prepareAppForLaunch(id:launchToken:)`
- keep launch failure cleanup through `cancelAppLaunch(...)`
- stop depending on a spawn handshake for PID or task ownership
- log that the container started detached

### Streaming response checklist

Inside the returned `StreamingServerResponse` producer:

- send the `.started` message first
- launch `docker logs -f <containerName>` using `swift-subprocess`
- consume subprocess output directly in structured concurrency
- write log chunks to the gRPC writer
- broadcast telemetry logs from the streamed text
- if the writer or request is cancelled, only cancel the log subprocess
- do not stop the Docker container on stream cancellation

### Practical note

If `docker logs -f` cannot preserve stdout and stderr separation cleanly, accept
that in the first implementation rather than reintroducing runtime ownership
complexity.

## 2.4 Refactor Docker stop behavior

### Update `stopTrackedAppIfRunning(id:)`

Keep native Darwin behavior unchanged.

For Docker-backed apps, replace the attached-task branch with Docker-driven
runtime control:

1. compute `containerName`
2. call `docker stop --time 10 <containerName>`
3. if needed, call `docker kill <containerName>`
4. mark stopped or reconcile state afterward

### Checklist

- do not wait on a stored Docker task
- do not cancel a stored Docker task
- if Docker reports the container is already absent, treat that as stopped
- keep the method idempotent for stopped or missing apps

## 2.5 Refactor Docker delete behavior

### Update `deleteContainer(...)`

Checklist:

- keep the existing stop-first behavior
- use `docker rm -f <containerName>` best-effort for Docker-backed apps
- remove the app record afterward
- publish updated app state

## 2.6 Add lightweight reconciliation helpers

Add small helpers before introducing a monitor.

### Suggested helpers

- `dockerContainerExists(appID:) async throws -> Bool`
- `dockerContainerIsRunning(appID:) async throws -> Bool`
- `refreshDockerAppStateIfNeeded(id:) async`
- `reconcileAllDockerApps() async`

### Use them from

- service startup or readiness path
- `listContainers` before building the response if inexpensive enough
- stop or delete recovery paths
- Docker start preflight or postflight if useful

### Behavior

For each registered Docker app:

- if the named container is running, ensure app state is `.running`
- if the named container is absent or exited, ensure app state is `.stopped`

## 2.7 Decide how to represent Docker `pid: nil`

The first implementation should not expose the Docker CLI PID.

Checklist:

- decide whether `WendyAppInfo.pid` can be left `nil` while status is `.running`
- if current assumptions require a PID for `.running`, update those assumptions
  in a narrow way rather than inventing a fake PID
- update tests accordingly

## 3. Optional in the same PR: Docker monitor

## `swift/WendyAgentCore/Sources/WendyAgent/Services/DockerContainerMonitor.swift`

Only add this after the detached-runtime and reconciliation work is already
stable.

### Minimum checklist

- run `docker events` filtered to Wendy-managed containers
- parse at least `start`, `die`, `stop`, and `destroy`
- route events back into `ContainerService` on actor isolation
- make monitor startup and shutdown explicit
- keep reconciliation as a fallback even if the monitor exists

If this makes the PR too large, it is the safest piece to defer.

## 4. Tests

## `swift/WendyAgentCore/Tests/WendyAgentTests/ContainerServiceTests.swift`

### Update existing expectations

- remove any assumptions that Docker-backed apps store a task handle
- remove any assumptions that Docker-backed running apps must expose a PID

### Add focused coverage

- starting a Docker-backed app marks it running without storing task state
- disconnecting the output stream does not mark the container stopped
- stop still works after a disconnected start stream
- delete still works without any stored Docker runtime handle
- reconciliation marks a disappeared container as stopped

### Testing strategy note

Prefer the existing fake `dockerExecutable` seam where possible so the first PR
can cover ownership semantics without requiring full Docker integration.

## 5. Manual verification checklist

Before considering the PR done, verify:

1. `StartContainer` starts a Docker-backed app and returns `.started`
2. output streaming works through `docker logs -f`
3. dropping the client stream does not stop the container
4. `StopContainer` still stops the detached container afterward
5. `DeleteContainer` removes the detached container afterward
6. listed app state converges to stopped after a spontaneous container exit
7. native Darwin app behavior still works as before

## 6. Out of scope for the first cut

Do not add these unless the first implementation is already stable:

- `docker attach --no-stdin --sig-proxy=false` for `StartContainer`
- stdin-aware `AttachContainer`
- real Docker container PID reporting via `docker inspect`
- moving native `Foundation.Process` state out of `WendyApp`
