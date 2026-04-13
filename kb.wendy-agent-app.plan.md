# WendyAgentApp plan

## Goal

Turn the Swift-based Wendy agent into a proper macOS app with a single window for configuration, while keeping the existing CLI working during the transition.

## Naming

Use explicit names for the three pieces:

- **Wendy Agent**: the shared logic/library extracted from the current CLI entry point
- **Wendy Agent CLI**: the command-line frontend
- **Wendy Agent App**: the native macOS app frontend

For code/project identifiers:

- Swift package library target/module: `WendyAgent`
- Swift package CLI target/module: `WendyAgentCLI`
- macOS app project name: `WendyAgentApp`
- macOS app target name: `WendyAgentApp`
- macOS app display name shown to users: `WendyAgent`

## High-level architecture

We will split the current Swift implementation into two frontends over one shared core:

1. **Wendy Agent CLI**
   - remains in `swift/Package.swift`
   - continues to parse CLI arguments and launch the agent
   - becomes a thin wrapper over the shared library

2. **Wendy Agent**
   - contains the actual agent logic currently assembled in `WendyAgent.swift`
   - exposes configuration, lifecycle, and status APIs reusable by both CLI and app
   - owns service composition, Docker detection, gRPC server startup, OTel collector startup, Bonjour advertising, and shutdown behavior

3. **Wendy Agent App**
   - native macOS app built in Xcode
   - uses the shared `Wendy Agent` library
   - provides a single window to configure and control the agent
   - uses `WendyAgentApp` as the Xcode project/target name and `WendyAgent` as the user-facing app display name

## Why this approach

- Lowest-risk migration path
- Keeps the current CLI behavior available while the app is being built out
- Avoids duplicating startup and service wiring logic
- Makes it easy to remove the CLI later if desired without affecting the core agent logic

## Proposed implementation phases

### Phase 1: extract shared Wendy Agent library

Refactor the existing CLI entry point so the startup logic moves into a reusable library target in the Swift package.

The shared library should define:

- an agent configuration model for everything currently passed via CLI
- an agent lifecycle type that can be started and stopped programmatically
- runtime/status reporting suitable for both CLI and app usage

The library should own:

- logging bootstrap strategy or configurable logging integration
- Docker availability checks and local registry setup
- gRPC server creation and startup
- local OpenTelemetry collector startup
- Bonjour advertiser startup
- service registration and shutdown coordination

### Phase 2: keep Wendy Agent CLI working on top of the library

Refactor the existing `wendy-agent` executable so it:

- still uses `swift-argument-parser`
- still accepts the current CLI options for now
- maps those options into the shared library configuration
- starts the shared library and waits for shutdown

This should preserve behavior while validating that the extraction was clean.

### Phase 3: create the native macOS app

Create an Xcode macOS app target/project for **WendyAgentApp** that depends on the local Swift package.

The app should:

- use `WendyAgentApp` as the Xcode project name
- use `WendyAgentApp` as the Xcode target name
- use `WendyAgent` as the app display name shown to users
- use a single main window
- use the shared library rather than duplicating startup logic

The app window should include controls for everything currently configurable through the CLI:

- main port
- OTel port
- config directory
- app executable path
- sandbox profile path

Even though `configDirectory` does not appear to be used yet in the current Swift implementation, it should remain part of the shared configuration for parity until we intentionally remove or repurpose it.

### Phase 4: app lifecycle and UI behavior

The app should own lifecycle explicitly rather than relying on CLI signal handling.

The first version should support:

- start/stop controls
- validation of configuration values
- visible running/stopped/error state
- presentation of startup failures in the window
- optional basic log/status output if it is cheap to expose from the shared library

The shared library API should be designed around programmatic control, with the CLI adapting to it rather than the library being designed around POSIX signals.

### Phase 5: later cleanup

Once the app is stable and covers the intended workflow, we can decide whether to:

- keep the CLI for development/debugging use
- reduce CLI scope
- remove the CLI entirely

That decision can be deferred.

## Project structure direction

### Swift package

Keep the Swift package as the home for:

- **WendyAgentCLI** executable frontend target
- **WendyAgent** shared library target
- existing generated gRPC/protobuf targets
- tests

### Xcode app

Create a separate native macOS app target/project for **WendyAgentApp** instead of trying to make the app itself a SwiftPM executable target.

Reasoning:

- app bundle metadata is more natural in Xcode
- signing, entitlements, assets, and Info.plist management are cleaner
- this gives a proper macOS app development workflow without disturbing the Swift package organization

## Key design constraints

### Shared lifecycle API

The extracted `Wendy Agent` library should support:

- constructing the agent from configuration
- starting asynchronously
- stopping programmatically
- surfacing status/errors back to callers

This is required for the app and improves the CLI design as well.

### Environment differences between CLI and app

A GUI app launched from Finder may not inherit the same shell environment as the CLI.

We should account for differences in:

- `PATH`
- Docker command discovery
- executable launch behavior
- file path assumptions

### Distribution model

The macOS app will likely need to be treated as a non-App-Store app because it:

- opens listening ports
- launches local processes
- may invoke Docker
- accesses files under user-controlled locations

## Initial app scope

The first version of the app should stay intentionally small:

- one window
- config form
- start/stop controls
- basic runtime state
- error handling
- optionally a simple log pane

Things like menu bar integration, launch-at-login, onboarding, or deeper provisioning UX should come later.

## Success criteria

We should consider this plan successful when:

1. the current `wendy-agent` executable still works with no meaningful behavior regression
2. the shared `Wendy Agent` library is the single place where the agent is assembled and run
3. the native macOS app with Xcode project/target name `WendyAgentApp` and display name `WendyAgent` can configure and start the agent from a single window
4. both frontends use the same underlying logic and configuration model

## Concrete target and module layout

### Naming conventions

Default naming should be `WendyAgent` / `Wendy Agent` across the library, CLI frontend, and app. Add qualifiers such as “CLI frontend”, “library”, or “app” only when needed for clarity.

For internal Swift target/module identifiers, disambiguation is still required where the toolchain demands unique names:

- Swift package library target/module: `WendyAgent`
- Swift package CLI target/module: `WendyAgentCLI` only because package target names must be unique
- executable product: `wendy-agent`
- app code module: `WendyAgentApp` only because the app cannot share a module name with the imported `WendyAgent` library

This keeps the public/product naming unified as WendyAgent while allowing internal module names to remain unambiguous.

### Swift package products

The Swift package should evolve to expose:

- a library product: `WendyAgent`
- an executable product: `wendy-agent`

Conceptually:

```swift
products: [
    .library(name: "WendyAgent", targets: ["WendyAgent"]),
    .executable(name: "wendy-agent", targets: ["WendyAgentCLI"]),
]
```

### Swift package targets

Recommended package target split:

- `WendyAgent`
  - shared library target
  - contains the extracted agent logic and public API
- `WendyAgentCLI`
  - executable target used for the WendyAgent CLI frontend
  - thin adapter around the shared library
- `WendyAgentTests`
  - tests for the shared library target
- existing generated gRPC/protobuf targets stay as they are:
  - `WendyAgentGRPC`
  - `WendyCloudGRPC`
  - `OpenTelemetryGRPC`

Conceptually:

```swift
targets: [
    .target(
        name: "WendyAgent",
        dependencies: [
            .product(name: "Logging", package: "swift-log"),
            .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
            .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
            .product(name: "GRPCCore", package: "grpc-swift-2"),
            .product(name: "GRPCServiceLifecycle", package: "grpc-swift-extras"),
            .target(name: "WendyAgentGRPC"),
            .target(name: "WendyCloudGRPC"),
        ],
        path: "Sources/WendyAgent"
    ),
    .executableTarget(
        name: "WendyAgentCLI",
        dependencies: [
            .target(name: "WendyAgent"),
            .product(name: "ArgumentParser", package: "swift-argument-parser"),
            .product(name: "Logging", package: "swift-log"),
        ],
        path: "Sources/WendyAgentCLI"
    ),
    .testTarget(
        name: "WendyAgentTests",
        dependencies: [
            .target(name: "WendyAgent"),
            .target(name: "WendyAgentGRPC"),
            .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
            .product(name: "GRPCCore", package: "grpc-swift-2"),
        ],
        path: "Tests/WendyAgentTests"
    ),
]
```

### Shared library source layout

Recommended source layout inside the Swift package:

```text
swift/
  Sources/
    WendyAgent/
      Core/
        Agent.swift
        AgentConfiguration.swift
        AgentState.swift
        AgentEvent.swift
        AgentRunInfo.swift
        AgentDirectories.swift
        AgentLogging.swift
      Bootstrap/
        AgentAssembly.swift
        AgentEnvironment.swift
        AgentServices.swift
      Docker/
        DockerCLI.swift
        DockerContainerBackend.swift
      Services/
        AgentService.swift
        AudioService.swift
        BonjourAdvertiser.swift
        ContainerService.swift
        FileSyncService.swift
        LocalOTelReceiver.swift
        OCITypes.swift
        ProvisioningService.swift
        TelemetryBroadcaster.swift
        TelemetryService.swift
```

Notes:

- do not use an `Internal/` folder
- keep the folder structure shallow: `category/file.swift`
- avoid arbitrarily deep hierarchies
- keep the public API concentrated in the `Core/` category
- existing service code can mostly be migrated with minimal logic changes

### CLI source layout

Recommended CLI layout:

```text
swift/
  Sources/
    WendyAgentCLI/
      CLI/
        main.swift
        CLIOptions.swift
        CLILogging.swift
        CLISignalHandling.swift
```

The CLI should be intentionally thin and mostly limited to:

- argument parsing
- mapping CLI options to `AgentConfiguration`
- logging setup appropriate for terminal usage
- signal handling that calls `Agent.stop()`

### Xcode app layout

Create a separate Xcode macOS app project that references the local Swift package.

Recommended layout:

```text
macos/
  Wendy Agent.xcodeproj
  Wendy Agent/
    App/
      WendyAgentApp.swift
    Model/
      AppSettings.swift
      SettingsValidation.swift
    Store/
      SettingsStore.swift
    ViewModel/
      AgentViewModel.swift
    View/
      MainWindowView.swift
      ConfigurationFormView.swift
      RuntimeStatusView.swift
      LogView.swift
    Resources/
      Assets.xcassets
    Support/
      Info.plist
```

Layout rule:

- keep code in a shallow `category/file.swift` structure
- do not introduce arbitrarily deep folder hierarchies

Recommended Xcode naming:

- project name: `Wendy Agent`
- app target display name: `Wendy Agent`
- product name: `Wendy Agent`
- Dock/app name: `Wendy Agent`
- internal Swift module name: `WendyAgentApp` only for compiler/module disambiguation

The app should import the local Swift package module `WendyAgent`.

## Main public types and proposed APIs

The shared library should expose a deliberately small, frontend-neutral public API.

### `AgentConfiguration`

Represents everything currently configurable through the CLI.

```swift
public struct AgentConfiguration: Sendable, Codable, Equatable {
    public var port: Int
    public var otelPort: Int
    public var configDirectory: URL
    public var appPath: URL?
    public var sandboxProfile: URL?

    public init(
        port: Int = 50051,
        otelPort: Int = 4317,
        configDirectory: URL = URL(fileURLWithPath: "/etc/wendy-agent"),
        appPath: URL? = nil,
        sandboxProfile: URL? = nil
    )

    public func validate() throws
}
```

Responsibilities:

- serve as the single shared configuration model used by both CLI and app
- retain `configDirectory` for parity even if it is not yet functionally used by the Swift implementation
- validate basic runtime correctness before startup

Suggested validation rules:

- ports must be in valid ranges
- `port` and `otelPort` must not collide
- file paths should be checked for existence/readability when provided
- `appPath` may be checked for executability

### `AgentDirectories`

Represents derived filesystem locations used by the running agent.

```swift
public struct AgentDirectories: Sendable, Equatable {
    public let applicationSupport: URL
    public let appsBase: URL
    public let blobsBase: URL

    public init(
        applicationSupport: URL,
        appsBase: URL,
        blobsBase: URL
    )
}
```

Purpose:

- expose effective runtime storage paths cleanly to both CLI and app
- allow the app UI to display derived locations if useful

### `DockerAvailability`

```swift
public enum DockerAvailability: Sendable, Equatable {
    case unknown
    case unavailable
    case available
}
```

Purpose:

- surface Docker state without exposing Docker implementation details

### `AgentRunInfo`

Represents the effective runtime info after startup.

```swift
public struct AgentRunInfo: Sendable, Equatable {
    public let port: Int
    public let otelPort: Int
    public let directories: AgentDirectories
    public let bonjourDisplayName: String
    public let bonjourDeviceID: String
    public let dockerAvailability: DockerAvailability
}
```

Purpose:

- provide callers with concrete, derived runtime facts once the agent is running
- give the app a clean model for status display without parsing logs

### `AgentFailure`

```swift
public struct AgentFailure: Sendable, Equatable, Error {
    public let message: String
}
```

Purpose:

- provide a small, stable failure type suitable for UI display and CLI printing

### `AgentState`

Represents coarse-grained lifecycle state.

```swift
public enum AgentState: Sendable, Equatable {
    case idle
    case starting
    case running(AgentRunInfo)
    case stopping
    case failed(AgentFailure)
}
```

Purpose:

- support both UI state rendering and CLI state reporting
- avoid requiring consumers to infer lifecycle from logs

### `AgentLogLevel`

```swift
public enum AgentLogLevel: String, Sendable, Equatable, Codable {
    case trace
    case debug
    case info
    case notice
    case warning
    case error
    case critical
}
```

### `AgentLogMessage`

```swift
public struct AgentLogMessage: Sendable, Equatable {
    public let date: Date
    public let level: AgentLogLevel
    public let subsystem: String
    public let message: String
}
```

Purpose:

- provide a frontend-neutral representation of log events
- allow the CLI to print logs and the app to render them in a simple pane

### `AgentEvent`

```swift
public enum AgentEvent: Sendable, Equatable {
    case stateChanged(AgentState)
    case log(AgentLogMessage)
}
```

Purpose:

- give both frontends a single stream of lifecycle and log updates
- avoid tying the shared library to SwiftUI, AppKit, or stderr directly

### `Agent`

This should be the primary public entry point.

```swift
public actor Agent {
    public init(configuration: AgentConfiguration)

    public var configuration: AgentConfiguration { get }

    public func state() -> AgentState

    public func start() async throws

    public func stop() async

    public func waitUntilStopped() async

    public func eventStream() -> AsyncStream<AgentEvent>
}
```

Expected behavior:

- `start()`
  - validates configuration
  - assembles services
  - checks Docker availability
  - starts the main gRPC server, OTel collector, and Bonjour advertiser
  - updates state and emits events
- `stop()`
  - performs graceful shutdown
  - is safe to call multiple times
- `waitUntilStopped()`
  - allows the CLI to block on the shared agent lifecycle cleanly
- `eventStream()`
  - allows either frontend to observe state transitions and logs

Rationale for making `Agent` an `actor`:

- lifecycle management is inherently stateful and concurrent
- actor isolation is a natural fit for start/stop/wait semantics
- it reduces the chance of invalid transitions or duplicate starts

## Internal shared-library types

These do not need to be public, but the implementation should likely center around them.

### `AgentAssembly`

Responsible for turning configuration into assembled services.

Conceptually:

```swift
struct AgentAssembly {
    func makeServices(configuration: AgentConfiguration) async throws -> AssembledAgent
}
```

### `AssembledAgent`

Private/internal structure holding the assembled runtime pieces.

Conceptually:

```swift
struct AssembledAgent {
    let runInfo: AgentRunInfo
    let start: @Sendable () async throws -> Void
    let stop: @Sendable () async -> Void
    let waitUntilStopped: @Sendable () async -> Void
}
```

This can be implemented differently in practice, but the idea is to create a clean seam between public lifecycle control and internal service composition.

### `AgentEnvironment`

Useful as an internal abstraction over environment-derived values and runtime probing, such as:

- host name
- home directory / application support paths
- Docker availability checks
- future test injection points

## CLI-specific types

The CLI should stay intentionally small.

### `CLIOptions`

Conceptually:

```swift
struct CLIOptions: ParsableArguments {
    @Option var port: Int = 50051
    @Option var otelPort: Int = 4317
    @Option var configDirectory: String = "/etc/wendy-agent"
    @Option var appPath: String = ""
    @Option var sandboxProfile: String = ""

    func makeAgentConfiguration() throws -> AgentConfiguration
}
```

Responsibilities:

- preserve the current CLI contract for now
- translate parsed options into the shared `AgentConfiguration`

### `main.swift`

Responsibilities:

- parse CLI args
- create `Agent`
- subscribe to `eventStream()` and print status/log output
- install signal handlers
- map `SIGINT` and `SIGTERM` to `Agent.stop()`
- call `waitUntilStopped()`

## App-specific internal types

These belong in the Xcode app target, not the shared library.

### `AppSettings`

A UI-friendly persistence model.

```swift
struct AppSettings: Codable, Equatable {
    var port: Int
    var otelPort: Int
    var configDirectory: String
    var appPath: String
    var sandboxProfile: String

    func makeAgentConfiguration() throws -> AgentConfiguration
}
```

Rationale:

- UI forms are often string-heavy and lenient while editing
- runtime configuration should stay strongly typed and validated
- keeping these separate makes validation and persistence cleaner

### `SettingsStore`

Conceptually:

```swift
final class SettingsStore {
    func load() -> AppSettings
    func save(_ settings: AppSettings)
}
```

Initial backing store recommendation:

- `UserDefaults` for editable app settings

### `AgentViewModel`

Conceptually:

```swift
@MainActor
final class AgentViewModel: ObservableObject {
    @Published var settings: AppSettings
    @Published private(set) var state: AgentState = .idle
    @Published private(set) var logs: [AgentLogMessage] = []
    @Published private(set) var errorMessage: String?

    func start()
    func stop()
    func saveSettings()
    func reloadSettings()
}
```

Responsibilities:

- bridge SwiftUI state to the shared `Agent`
- manage an event-stream consumption task
- update published state for the window
- expose start/stop behavior to the UI

## Source migration plan

### Current state

Today, `swift/Sources/WendyAgent/WendyAgent.swift` effectively combines:

- CLI entrypoint behavior
- option parsing
- runtime assembly
- startup execution

### Proposed migration

1. move the runtime assembly logic out of the current `WendyAgent.swift`
2. place public lifecycle/configuration types in the new shared `WendyAgent` library surface
3. keep implementation-heavy service code in shallow category folders such as `Bootstrap/`, `Docker/`, and `Services/`
4. create `swift/Sources/WendyAgentCLI/` for the new CLI wrapper target used by the WendyAgent CLI frontend
5. make the old CLI behavior call into the shared `Agent` actor

This sequence allows the CLI to continue working while proving out the shared boundary needed by the macOS app.

## API design principles

The shared `Wendy Agent` library should:

- remain frontend-neutral
- avoid importing SwiftUI or AppKit
- avoid owning persistence concerns such as `UserDefaults`
- avoid exposing internal services such as `ContainerService` or `FileSyncService`
- expose one small lifecycle-oriented API surface centered on `Agent`

The app should own:

- settings persistence
- form validation UX
- window state
- log presentation

The CLI should own:

- terminal logging behavior
- signal handling
- argument parsing

## Example usage shape

### CLI side

Conceptually:

```swift
let configuration = try options.makeAgentConfiguration()
let agent = Agent(configuration: configuration)

Task {
    for await event in agent.eventStream() {
        // print state/logs to terminal
    }
}

try await agent.start()
await agent.waitUntilStopped()
```

### App side

Conceptually:

```swift
let configuration = try settings.makeAgentConfiguration()
let agent = Agent(configuration: configuration)

Task {
    for await event in agent.eventStream() {
        // bind state/logs into the UI
    }
}

try await agent.start()
```

The same shared lifecycle model should serve both frontends cleanly.
