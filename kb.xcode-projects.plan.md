# Xcode Project Support

## Background

`wendy run` for macOS targets currently assumes every Swift project is managed by SwiftPM. It detects projects by looking for `Package.swift`, discovers the product name via `swift package dump-package`, and builds with `swift build`. Some libraries -- most notably MLX -- cannot be built with SwiftPM because SwiftPM does not support copying Metal shader resource bundles into build products. These projects require an Xcode project (`.xcodeproj`) and must be built with `xcodebuild`.

## Plan

### Detection

`detectProjectType()` gains a check for `*.xcodeproj` directories in the **current directory only**, returning a new `"xcode"` project type. Precedence rules:
- If `Package.swift` is present, SwiftPM wins regardless of any `.xcodeproj` (returns `"swift"`).
- If exactly one `*.xcodeproj` is found and no `Package.swift`, returns `"xcode"`.
- If multiple `*.xcodeproj` directories are found, returns an error.

`detectBuildOptions()` also gains the check, adding a `BuildOption` entry per discovered `*.xcodeproj` to the list surfaced in the interactive picker. `runWithAgent()` dispatches into the new Xcode path the same way it currently dispatches into `runMacOSWithAgent()` for `"swift"` + platform darwin. Detection is pure filesystem logic and follows the exact same `t.TempDir()` pattern already used in `docker_test.go` for the existing project types, with tests covering: Xcode-only, SwiftPM-wins-when-both-present, and multiple-xcodeproj-error.

### Scheme discovery

A new `findXcodeScheme()` sits alongside `findSwiftProduct()`. It shells out to `xcodebuild -list -json` via `execCommandContext` (same injectable command runner already used throughout the package) and passes the raw output to a pure `parseXcodeSchemes([]byte) ([]string, error)` helper. Separating parsing from invocation means `parseXcodeSchemes` can be unit-tested with fixture JSON without Xcode installed, and `execCommandContext` injection covers the invocation paths (scheme found, not found, xcodebuild missing) the same way `ensureSwiftVersion` is tested today. One scheme -> use it automatically, multiple -> error with a hint to set `"scheme"` in `wendy.json`. `AppConfig` gets an optional `"scheme"` field as the escape hatch. `ensureAppConfig` is updated so that the `"xcode"` project type also sets `language: "swift"` (same as the SwiftPM path).

### Build

`runMacOSWithAgent()` is renamed to `runMacOSSwiftPMWithAgent()`. A new `runMacOSXcodeWithAgent()` sits alongside it. It runs `xcodebuild` with `-configuration Release` and `-derivedDataPath .xcode/` (relative to the project directory, the same directory that contains `wendy.json` and the `.xcodeproj`). Architecture and code signing are not overridden -- `xcodebuild` uses whatever is configured in the project's build settings. The scheme is always supplied via `-scheme`; `-target` is not used. A pure `findXcodeBuildProduct(derivedDataPath, scheme string)` helper (configuration is always `Release`) inspects the build products directory and returns the product path and whether it is a plain binary or a `.app` bundle. Because this is pure filesystem inspection it is fully testable with `t.TempDir()`: tests create a fake build products directory containing either a plain binary or a `*.app` structure and assert the correct path and type are returned.

### Transfer

Transfer uses `syncFiles()` exactly as the SwiftPM path does -- no OCI packaging involved. What goes into the `fileSyncEntry` list differs by product type:

- **Command-line tool** -- the binary is added as a file entry, exactly as in the SwiftPM path. All `.bundle` directories sitting next to the binary in the build products directory are added as additional directory entries; `syncFiles` walks them recursively.
- **`.app` bundle** -- the entire `.app` directory is added as a single directory entry with `remotePath: "<Name>.app"`, landing in the same base directory as a CLI binary would; `syncFiles` walks the full bundle tree, covering `Contents/Resources`, `Contents/Frameworks`, and everything else.

In both cases `sandbox.sb` and user-declared `files` entries from `wendy.json` are appended to the sync list as usual. The `syncFiles` function itself is already thoroughly tested via the in-process fake gRPC server pattern in `filesync_test.go`; new tests here only need to cover the entry-assembly logic (given a build products directory with a binary and sibling bundles, assert the correct `fileSyncEntry` slice is produced).

### `CreateContainerRequest`

`Cmd` derivation is extracted into a pure `xcodeEntrypoint(productPath string, isAppBundle bool) string` function. For a CLI tool it returns the binary name; for a `.app` bundle it returns `<Name>.app/Contents/MacOS/<Name>`, where `<Name>` is derived from the scheme name (no `Info.plist` parsing for now). Being a pure function it is trivially unit-tested with a table of inputs and expected outputs.

## Demo Project

Before end-to-end testing is possible, a `HelloXcode` demo project must be created manually using the Xcode GUI. It should be a minimal command-line tool (no `Package.swift`) that exercises the Xcode-only build path -- ideally with at least one resource bundle alongside the binary. Place it under `Examples/HelloXcode/`.

## Acceptance Criteria

- `wendy run` in `Examples/HelloXcode` (`.xcodeproj`, no `Package.swift`) builds with `xcodebuild`, syncs the binary and any sibling `.bundle` directories to the target, and runs successfully end-to-end.
- `wendy run` in a project that produces a `.app` bundle syncs the full bundle and launches the binary inside it.
- Existing SwiftPM examples (e.g. `HelloMac`) are unaffected by the rename.
- Clear error when `xcodebuild` is not found in `PATH`.
- All logic that does not require a live device or Xcode installation is covered by unit tests.
