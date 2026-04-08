# Plan: `run.args` in `wendy.json` for Mac native apps

## Goal

Allow users to configure launch arguments for Mac native apps in `wendy.json`
under a `run` key. These arguments are passed to the native executable when
Wendy launches it through the Mac native run flow.

## Scope

This change only covers the Mac native path:
- Xcode projects run through the Mac agent
- SwiftPM projects run through the Mac agent
- Native executable launch in the Swift Mac agent's file-sync path

## Out of scope

Everything outside the Mac native path is out of scope for this change,
including:
- Linux
- Mac container-based runs
- Dockerfile argument handling
- CLI passthrough arguments such as `--`
- CLI `--user-args` support on Mac native paths
- Argument precedence rules across multiple sources
- Legacy OCI or image-based launch paths in the Mac agent
- `run.env`

## `wendy.json`

Add a `run.args` field:

```json
{
  "appId": "my-app",
  "run": {
    "args": ["--verbose", "--port", "8080"]
  }
}
```

Semantics:
- `run.args` is an array of command-line arguments
- Each array element becomes exactly one argv element passed to the app
- Wendy does not invoke a shell and does not split arguments on whitespace
- `run.args: []` is valid and means launch the app with no app arguments
- `run` may be omitted entirely
- `run` must be an object when present; `run: null` is invalid
- `run.args` must be an array when present; `run.args: null` is invalid

Examples:
- `["--port", "8080"]` passes two arguments
- `["--message", "hello world"]` passes `hello world` as a single argument
- `["--message=hello world"]` passes one argument containing a space

## Background

The Mac native run path builds locally, syncs the built product to the Mac
agent, then calls `CreateContainer` + `StartContainer` over gRPC. The agent
launches the binary using `Foundation.Process`.

Currently:
- `CreateContainerRequest` has a `user_args` proto field
- the Mac native Xcode and SwiftPM paths do not populate that field
- the Swift Mac agent does not store or apply `userArgs` in the file-sync
  native launch path
- as a result, configured launch arguments are not passed to the app

## Design

### 1. `go/internal/shared/appconfig/appconfig.go`

Add `RunConfig` and wire it into `AppConfig`:

```go
// RunConfig holds runtime configuration applied when the app is started.
type RunConfig struct {
    Args []string `json:"args,omitempty"`
}

// In AppConfig:
Run *RunConfig `json:"run,omitempty"`
```

No validation is required beyond JSON parsing.

### 2. `go/internal/shared/appconfig/wendy.schema.json`

Add `run.args` to the schema so editors can autocomplete and validate it.

Expected shape:
- `run` is an object
- `run` uses `additionalProperties: false`
- `run.args` is an array
- each item in `run.args` is a string
- `run: null` is rejected by the schema
- `run.args: null` is rejected by the schema

### 3. `go/internal/cli/commands/xcode.go`

In `runMacOSXcodeWithAgent`, populate `CreateContainerRequest.UserArgs` from
`appCfg.Run.Args`:

Notes:
- This change only uses `wendy.json` `run.args`
- Existing CLI `--user-args` remains ignored on this Mac native path

```go
var runArgs []string
if appCfg.Run != nil {
    runArgs = appCfg.Run.Args
}
createReq := &agentpb.CreateContainerRequest{
    AppName:  appCfg.AppID,
    Cmd:      xcodeEntrypoint(productPath, isApp),
    UserArgs: runArgs,
}
```

### 4. `go/internal/cli/commands/run.go`

In `runMacOSSwiftPMWithAgent`, make the same change and populate
`CreateContainerRequest.UserArgs` from `appCfg.Run.Args`.

Notes:
- This change only uses `wendy.json` `run.args`
- Existing CLI `--user-args` remains ignored on this Mac native path

### 5. `swift/Sources/WendyAgent/Services/ContainerService.swift`

Update only the file-sync/native launch path.

#### `createContainer`

In the branch where `imageName` is empty and `cmd` carries the native binary
name, store `userArgs` together with the app directory and binary name.

Only this file-sync branch is updated to store launch args. Legacy OCI and
other native image-based branches remain unchanged for now.

```swift
private var appDirectories: [String: (directory: String, binaryName: String, args: [String])] = [:]
```

```swift
appDirectories[appName] = (
    directory: appDirectory,
    binaryName: cmd,
    args: Array(request.message.userArgs)
)
```

#### `startContainer`

Apply the stored args to the launched process.

Even when the stored args are empty, set `process.arguments` explicitly for
deterministic launch behavior.

Non-sandboxed launch:

```swift
process.executableURL = URL(fileURLWithPath: binaryPath)
process.arguments = entry.args
```

Sandboxed launch:

```swift
process.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
process.arguments = ["-f", profilePath, binaryPath] + entry.args
```

Note: `run.args` applies to the app's argv. Required wrapper arguments used to
launch sandboxed apps remain in place.

## Testing

- Add a unit test in `appconfig_test.go` covering parse + round-trip of
  `run.args`
- Include at least these cases:
  - no `run`
  - `run.args: ["--verbose"]`
  - `run.args: []`
- Add Go CLI tests covering Mac native request wiring for:
  - `runMacOSXcodeWithAgent`
  - `runMacOSSwiftPMWithAgent`
- Manual test with a small CLI or app that prints `CommandLine.arguments` /
  `os.Args`
- Verify arguments are passed correctly for:
  - separate args such as `("--port", "8080")`
  - values containing spaces such as `"hello world"`
  - empty args array
