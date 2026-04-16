# WendyAgent apps lifecycle, persistence, and observation plan

## Summary

Add first-class app lifecycle management to `WendyAgent`, expose all known apps publicly, persist them to disk, and make agent shutdown stop all running apps before shutting down services.

This plan introduces:

- a public `WendyAppInfo` value type
- a public `WendyAgent.apps: [WendyAppInfo]`
- `WendyAgent.observeApps(...)`
- `WendyAgent.stopApp(id:)`
- `WendyAgent.stopAllApps()`
- internal unified app model `WendyApp`
- app persistence in a single file: `info.json`
- live synchronization between runtime state and `WendyAgent.apps`
- automatic status updates when apps exit on their own
- shutdown behavior where `WendyAgent.stop()` stops all running apps before stopping servers
- a semantic change so app listing means all known apps, not only running ones

Terminology will consistently use **apps** and **stop**.

---

## Agreed semantics

### Public app list semantics

`apps` means **all known apps**, not only currently running apps.

Each app carries a `status`:

- `.running`
- `.stopped`

This means:

- `WendyAgent.apps` returns all known apps
- `observeApps()` observes all known apps
- CLI/API app list behavior should be updated to match this new meaning

### App stop semantics

- `stopApp(id:)` is a **no-op** if the app is not currently running
- `stopAllApps()` stops only currently running apps, but known apps remain in `apps` with status `.stopped`
- `stopAllApps()` and `WendyAgent.stop()` wait for apps to actually exit before returning
- native apps are terminated gracefully first, then force-killed if needed after a bounded timeout
- an app remains `.running` until it has actually exited

### Persistence semantics

- known apps are persisted
- on startup, persisted apps are restored as `.stopped`
- we will also persist last-known runtime fields such as PID to support future reconciliation work
- startup will **not** try to rediscover and reattach to surviving runtime processes in this implementation

### Known app semantics

An app becomes known once it is successfully registered/created by the agent, and remains known until explicitly removed.

That means:

- created but never started -> known, `.stopped`
- started then stopped -> known, `.stopped`
- removed -> no longer known

---

## Non-goals

1. Enumerating arbitrary host OS processes not managed by Wendy.
2. Reattaching to surviving native/container processes after agent restart.
3. Making child process lifetime depend on parent death for crash scenarios.
4. Building a full migration/versioning system for app persistence beyond what is needed now.

---

## Public API design

### `WendyAppInfo`

Add a new public value type in `WendyAgentCore/Sources/WendyAgent/`:

```swift
public struct WendyAppInfo: Sendable, Equatable {
    public enum Kind: Sendable, Equatable {
        case native
        case container
    }

    public enum Status: Sendable, Equatable {
        case stopped
        case running
    }

    public let id: String
    public let kind: Kind
    public let status: Status
    public let pid: Int32?
}
```

Notes:

- `id` is the app ID from `wendy.json` / CLI side
- `.container` is the public abstraction, not `.docker`
- `pid` for `.container` is the host-side attached runtime PID
- `status` is part of the public model because `apps` contains both running and stopped apps

### `WendyAgent`

Add public API:

- `public private(set) var apps: [WendyAppInfo] = []`
- `public func observeApps(_ handler: ...) -> WendyObservation`
- `public func stopApp(id: String) async`
- `public func stopAllApps() async`

Notes:

- `stopApp(id:)` is non-throwing because stopping a non-running app is a no-op and stop failures should be handled internally/logged consistently
- `apps` should always be sorted by `id` for stable observation behavior
- `observeApps()` should mirror `observeStatus()` including initial value delivery

---

## Internal model design

### Internal `WendyApp`

Inside `ContainerService`, introduce an internal unified model for a known app:

```swift
struct WendyApp: Sendable, Codable {
    var info: WendyAppInfo

    // Durable app metadata
    var native: NativeMetadata?
    var container: ContainerMetadata?

    // Transient runtime-only fields
    var process: Foundation.Process?
    var launchToken: UUID?
}
```

Intent:

- `info` is the canonical public-facing projection
- `WendyApp` is the canonical internal model
- `info.json` should mirror `WendyApp`, omitting only truly transient runtime-only fields
- `process` is present only while the app is actually running
- `launchToken` protects against stale termination callbacks

This internal type is the single in-memory source of truth per known app and also defines the durable shape written to disk, minus transient runtime-only fields.

### In-memory storage

Replace runtime-only tracking with a unified known-app map:

```swift
private var appsByID: [String: WendyApp] = [:]
```

This map is the source of truth for:

- `WendyAgent.apps`
- persistence to `info.json`
- CLI/gRPC app listing
- app stop/start bookkeeping

---

## Persistence design

### File location

Persist all known apps in a single file:

```text
~/Library/Application Support/wendy-agent/info.json
```

This lives alongside the agent's other application-support data.

### Persisted model

Persist `WendyApp` directly, omitting only transient runtime-only fields:

- `process`
- `launchToken`

This can be implemented either by:

- making `WendyApp` manually `Codable`, or
- using an internal codable payload nested in `WendyApp`

but the important rule is that `info.json` should mirror the internal `WendyApp` model rather than introduce a second parallel persisted schema.

### Durable app metadata

The persisted `WendyApp` should include the metadata currently needed to start/remove the app later.

For native apps this includes:

- app directory
- binary name
- args
- current directory
- any other launch metadata already captured in `NativeLaunchInfo`

For container apps this includes:

- image name
- raw or re-encodable app config data
- any other metadata currently needed to start/remove the container app later

### Runtime persistence

We will also persist last-known runtime fields for future use, including:

- last known `status`
- last known `pid`

Even though startup will currently restore all apps as `.stopped`, these fields will be available for future reconciliation logic.

### Startup restore behavior

On startup:

1. load `info.json` if present
2. rebuild `appsByID`
3. set every restored app's public status to `.stopped`
4. clear runtime attachments (`process = nil`, `launchToken = nil`)
5. publish `apps`

No runtime reattachment is attempted in this iteration.

### Save behavior

Persist after every mutation that changes known app membership or known app metadata, including:

- create/register
- start if status/pid should be reflected on disk
- stop completion
- remove
- spontaneous process exit changing status to `.stopped`

A single helper should own writing the file, e.g.:

- `saveApps()`
- `loadApps()`

Writes should be atomic.

---

## `ContainerService` responsibilities

`ContainerService` remains the owner of app lifecycle behavior.

### New/refactored helpers

Refactor toward a small set of explicit transition helpers:

- `private func currentAppInfos() -> [WendyAppInfo]`
- `private func publishApps() async`
- `private func loadApps() throws`
- `private func saveApps() throws`
- `private func registerApp(...) async throws`
- `private func markAppRunning(id: String, process: Foundation.Process, launchToken: UUID) async throws`
- `private func markAppStopped(id: String) async`
- `private func stopApp(id: String) async`
- `private func stopAllApps() async`
- `private func removeApp(id: String) async throws`
- `private func handleAppTermination(id: String, launchToken: UUID) async`

These transition helpers should own:

- mutating `appsByID`
- saving `info.json`
- publishing app updates

The gRPC handlers should become thin adapters over this shared logic.

### `currentAppInfos()`

Returns:

- `appsByID.values.map(\.info)`
- sorted by `id`

This is the canonical snapshot used by:

- `WendyAgent.apps`
- app observation
- gRPC/CLI listing

### `publishApps()`

Calls the callback provided by `WendyAgent` with the latest `[WendyAppInfo]`.

Call this after every app state mutation, including:

- load on startup
- create/register
- start success
- stop completion
- remove
- app exit on its own

### `registerApp(...)`

Used by `createContainer` and any equivalent registration path.

Behavior:

1. derive app identity and metadata
2. insert or update `appsByID[id]`
3. set public status to `.stopped`
4. clear transient runtime state
5. persist to disk
6. publish apps snapshot

This is the point where an app becomes known.

### `markAppRunning(...)`

Shared running transition should:

1. resolve the app from `appsByID`
2. update `info.status = .running`
3. update `info.pid`
4. set `process` and `launchToken`
5. install `terminationHandler`
6. persist updated last-known runtime info
7. publish apps snapshot

### `stopApp(id:)`

Shared single-app stop logic used by:

- `WendyAgent.stopApp(id:)`
- gRPC/CLI stop
- `stopAllApps()`
- agent shutdown

Behavior:

1. look up `appsByID[id]`
2. if app is missing or not running, return (no-op)
3. if `.native`:
   - call `terminate()`
   - await exit with a bounded timeout
   - if still alive, force kill
4. if `.container`:
   - call the existing backend stop path
   - await attached process exit
5. only once exit is confirmed:
   - call `markAppStopped(id:)`

Apps remain `.running` until actual exit.

### `markAppStopped(id:)`

Shared stopped transition should:

1. clear `process` / `launchToken`
2. set `info.status = .stopped`
3. set `info.pid = nil`
4. persist
5. publish apps snapshot

### `stopAllApps()`

Behavior:

1. snapshot all IDs whose status is `.running`
2. stop each app using shared `stopApp(id:)`
3. continue on failures/timeouts after logging
4. persist final state
5. publish final apps snapshot

This method is best-effort and non-throwing.

### `handleAppTermination(id:launchToken:)`

Used by both native and container runtime processes.

Behavior:

1. look up `appsByID[id]`
2. only apply if the stored `launchToken` matches
3. call `markAppStopped(id:)`

This prevents stale termination callbacks from a previous launch removing/changing a newer launch.

### `removeApp(id:)`

Shared remove logic should:

1. stop the app first if currently running
2. allow the visible transition from `.running` to `.stopped`
3. remove durable app metadata/resources as appropriate
4. remove from `appsByID`
5. persist
6. publish apps snapshot

This makes removed apps no longer known and intentionally exposes the two-step lifecycle:

- `.running` -> `.stopped` -> removed

---

## Synchronization from `ContainerService` to `WendyAgent`

### Callback from service to agent

When constructing `ContainerService`, pass a callback:

```swift
onAppsChanged: @Sendable ([WendyAppInfo]) async -> Void
```

`ContainerService` uses this callback only to publish snapshots.

Implementation detail:

- avoid retain cycles between `WendyAgent` and `ContainerService`
- weak capture or a lightweight proxy is acceptable

### `WendyAgent` update method

Add:

```swift
private func updateApps(_ apps: [WendyAppInfo])
```

Behavior:

- compare with current `apps`
- if unchanged, do nothing
- otherwise assign and notify observers

This should mirror `updateStatus(_:)`.

---

## Observation support in `WendyAgent`

Add observer infrastructure parallel to status observation:

- `private var appsObservationRegistry = WendyObservationRegistry<[WendyAppInfo]>(...)`
- `private var appsObservationTasks: [...] = [:]`
- `public func observeApps(...) -> WendyObservation`
- scheduling/delivery/cancellation helpers matching the status implementation

Requirements:

- initial value delivery
- main-actor delivery
- only notify on actual value change

---

## Launch and runtime flow changes

### Native app launch

When a native app is successfully started:

1. create `WendyAppInfo(id: ..., kind: .native, status: .running, pid: process.processIdentifier)`
2. update `appsByID[id]`
3. set `process` and `launchToken`
4. install `terminationHandler`
5. persist last-known runtime info
6. publish apps snapshot

### Container app launch

When a container app is successfully started via the existing Docker-backed path:

1. create `WendyAppInfo(id: ..., kind: .container, status: .running, pid: process.processIdentifier)`
2. update `appsByID[id]`
3. set `process` and `launchToken`
4. install `terminationHandler`
5. persist last-known runtime info
6. publish apps snapshot

---

## gRPC / CLI behavior changes

### App list semantics

Refactor app listing so it returns **all known apps**, not only currently running apps.

Mapping from `WendyAppInfo` to existing protobuf should include:

- `appName = info.id`
- `runningState = info.status == .running ? .running : .stopped`

This intentionally changes behavior from the currently documented CLI help text.

### Docs/help update required

The CLI/help/docs that currently say `list` means "List running applications" must be updated to reflect the new semantics:

- `list` -> list known applications and their status

### Stop behavior

Refactor `StopContainer` / app stop handling to call shared `stopApp(id:)`.

This keeps behavior aligned across:

- Swift API
- gRPC API
- CLI
- agent shutdown

---

## `WendyAgent` lifecycle changes

### Store `ContainerService`

Add:

```swift
private var containerService: ContainerService?
```

Startup should:

1. create `ContainerService`
2. wire `onAppsChanged`
3. store it on `WendyAgent`
4. include it in the gRPC services array

### Public `apps`

Add:

```swift
public private(set) var apps: [WendyAppInfo] = []
```

This is populated:

- from persisted state loaded during startup
- then kept live by `ContainerService`

### `stopApp(id:)`

Add:

```swift
public func stopApp(id: String) async {
    await self.containerService?.stopApp(id: id)
}
```

### `stopAllApps()`

Add:

```swift
public func stopAllApps() async {
    await self.containerService?.stopAllApps()
}
```

### `stop()` behavior

Update stop order to:

1. guard current status
2. set status to `.stopping`
3. stop monitor task
4. tell `ContainerService` to reject new starts/mutations
5. `await self.containerService?.stopAllApps()`
6. stop Bonjour
7. stop OTel server
8. stop main gRPC server
9. clear runtime state
10. set status to `.stopped`

Rationale:

- app streams can complete naturally before server shutdown
- `stop()` returns only after running apps have actually stopped

### Clear runtime state

Ensure `clearRuntimeState()` also clears:

- `containerService`
- observation task state if needed

Do **not** clear persisted known apps from disk as part of normal stop.

`apps` should remain the known-app view in memory during the agent lifetime; after a fresh start they reload from disk.

---

## Stop-state gating

Add internal stop-state gating in `ContainerService`, e.g.:

```swift
private var isStopping = false
```

Add helper:

- `func beginStopping()`

Reject mutating lifecycle operations while stopping, such as:

- create/register
- start
- attach

Read-only operations remain allowed:

- app list
- stats

---

## File-by-file change outline

### New files

Potential additions:

- `WendyAgentCore/Sources/WendyAgent/WendyAppInfo.swift`
- `WendyAgentCore/Sources/WendyAgent/WendyApp.swift` (or similar)

### `WendyAgent.swift`

Add/change:

- `apps`
- app observation machinery
- `observeApps(...)`
- `stopApp(id:)`
- `stopAllApps()`
- `updateApps(_:)`
- stored `containerService`
- startup wiring for app callbacks
- stop-order changes

### `ContainerService.swift`

Add/change:

- internal `WendyApp`
- `appsByID`
- persistence helpers (`loadApps`, `saveApps`)
- unified shared app lifecycle helpers
- process termination handling
- update list/stop/remove behavior to use shared app logic
- remove old assumptions that only running native apps are known

### Existing support types

Extend/adapt:

- native launch metadata types to be codable/persistable
- container metadata handling to persist what start/remove needs

### CLI/docs/help text

Update app list command help/docs to reflect new semantics.

---

## Test plan

### `WendyAgent` tests

1. `apps` starts empty when no persisted file exists.
2. persisted apps load on startup as `.stopped`.
3. starting an app updates `apps` status to `.running`.
4. stopping an app updates status to `.stopped` but keeps the app known.
5. `observeApps()` receives initial and subsequent values.
6. `stopAllApps()` waits for running apps to exit and leaves them as `.stopped`.
7. `stop()` stops all running apps before server shutdown.

### `ContainerService` tests

1. registering/creating an app adds it to `appsByID` and persists it.
2. native app launch sets status `.running` and PID.
3. container app launch sets status `.running` and host-side PID.
4. native process exit flips status to `.stopped` and clears PID.
5. container attached process exit flips status to `.stopped` and clears PID.
6. stale termination callback does not overwrite a newer launch.
7. `stopApp(id:)` is a no-op for missing/stopped apps.
8. native stop escalates to force kill after timeout.
9. `stopAllApps()` continues if one app fails to stop cleanly.
10. `remove` first transitions a running app to `.stopped`, then removes it from known apps and persistence.
11. transition helpers are the only code paths that mutate app state, save, and publish.

### Persistence tests

1. `info.json` is created atomically.
2. last-known PID/status are persisted.
3. startup restore resets all restored apps to `.stopped`.
4. corrupted or missing `info.json` is handled gracefully.

### Behavior regression checks

1. CLI/gRPC app stop still works through the shared stop path.
2. app list returns all known apps with correct running/stopped mapping.

---

## Open implementation details

1. Exact native stop timeout duration before force kill.
2. Whether the durable app metadata should be encoded directly by `WendyApp` or by a nested codable payload inside `WendyApp`.
3. Whether `stopApp(id:)` should log missing IDs or silently no-op.
4. Whether app list protobuf/message names should eventually be renamed for clarity, or just reuse current surface with new semantics.

---

## Recommended implementation order

1. Add `WendyAppInfo` with `Kind` and `Status`.
2. Add `apps` state and `observeApps()` to `WendyAgent`.
3. Store `ContainerService` on `WendyAgent` and wire `onAppsChanged` callback.
4. Introduce internal unified `WendyApp` and `appsByID`.
5. Add `info.json` load/save helpers around `WendyApp` persistence.
6. Restore persisted apps on startup as `.stopped`.
7. Introduce explicit transition helpers (`registerApp`, `markAppRunning`, `markAppStopped`, `removeApp`).
8. Refactor create/register paths to create known apps and persist them.
9. Refactor native/container start paths to update shared app state.
10. Add termination-handler based status transitions.
11. Refactor shared `stopApp(id:)`, `stopAllApps()`, and remove logic.
12. Update app list behavior to return all known apps.
13. Update `WendyAgent.stop()` ordering.
14. Update CLI/docs/help text.
15. Add tests.

---

## Expected outcome

After this change:

- `WendyAgent` exposes all known apps via `apps`.
- each app reports `running` or `stopped` status.
- observers can react to app registration, start, stop, remove, and spontaneous exit.
- app metadata survives agent restarts via `info.json`.
- `stop()` stops all running apps first, then shuts down servers.
- remove follows the explicit visible lifecycle `.running` -> `.stopped` -> removed when needed.
- Swift API, gRPC, CLI, and persistence all share one app model and one lifecycle path.
