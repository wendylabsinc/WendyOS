# Wendy Core for Swift

The SwiftPM product `WendyCore`: a thin Swift layer over the C interface, separate from the Wendy Swift SDK ([wendy-swift](https://github.com/wendylabsinc/wendy-swift)), which builds on it. Its local manifest is `go/wendy-core/Package.swift`. SwiftPM resolves remote packages from repository roots, so this nested package cannot be consumed by a WendyOS URL.

```swift
import WendyCore
let result = WendyHelloWorld() // Int32(42)
```

The Go engine is linked statically: Go `c-archive` libraries in an XCFramework for macOS 15+ (arm64/x64), iOS 18+ and the iOS Simulator (arm64/x64), and in a SwiftPM artifact bundle for Linux x64 (glibc 2.35+) and arm64 (glibc 2.39+). The C header is exposed to Swift as the `CWendyCore` module. Consumers need no Go toolchain. Swift 6.2+ is required. Other Swift-capable platforms are not automatically supported.

Each process should link at most one Go-based library: a second embedded Go runtime causes duplicate symbols or undefined behavior.

## Versions and binaries

Remote Swift distribution from WendyOS is an open design question. Recommend
checksummed XCFramework/artifact-bundle `binaryTarget` URLs in WendySwift from
future WendyOS Core releases; a publish-only mirror is an alternative. The
separate WendySwift repository and its existing Core dependency stay unchanged.
No tags, binaries or package versions are published by this migration.

For local development, run `make swift` from `go/wendy-core/`. This writes the
ignored `distributions/swift/artifacts/`: the Apple XCFramework on macOS or the
host artifact bundle on Linux. Depend on this directory by local path.

`WendyCoreVersion() -> String` returns the effective version stamped into the linked engine, including preview or development suffixes. Source development identities use `0.1.0-dev+<7-character commit>` when a commit is known; the packaged build identity takes precedence. Local checkout builds stamp the same `-dev+<7-character HEAD commit>` format, without a dirty-tree marker.

The first SDK release is `0.1.0-seed.1`. A requirement starting at that prerelease can select it; `from: "0.1.0"` cannot, because the seed precedes the final version.
