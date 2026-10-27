# Wendy Core

`wendy-core` is the independently versioned release unit in `WendyOS/go/wendy-core` for the portable engine, selected modules, public contracts and included platform/subsystem implementations. Go is the chosen engine implementation. Core ships in language-consumable distributions. The ergonomic language SDKs build on them in their own repositories: [Wendy Swift](https://github.com/wendylabsinc/wendy-swift) and [Wendy Go](https://github.com/wendylabsinc/wendy-go); others are not started.

The runnable contract currently exports `WendyHelloWorld()`, returning the integer **42**, and `WendyCoreVersion()`, returning the effective engine version. Go callers use `core.WendyHelloWorld()` and `core.Version()` directly. Every incarnation reaches the same Go engine. No device, Cloud, authentication, or platform-capability behavior is implemented.

Core remains a nested Go module, `github.com/wendylabsinc/WendyOS/go/wendy-core`.
Run Core commands from this directory; root-module `go test ./...` skips it.
The standalone [wendy-go](https://github.com/wendylabsinc/wendy-go) and
[wendy-swift](https://github.com/wendylabsinc/wendy-swift) repositories remain
unchanged until follow-up migrations. `Package.swift` supports local builds;
SwiftPM cannot fetch a package in a repository subdirectory. Distribution from
WendyOS is an open design question; see [release boundaries](documentation/releasing.md).

## Shape

```text
core/                    Public Go incarnation; no C round trip
  runtime/               Shared runtime foundation placeholder
  identity/              Proposed domain placeholder
  devices/               Proposed domain placeholder
  applications/          Proposed domain placeholder
  events/                Proposed domain placeholder
cmd/wendy-core-native/   Go engine exported through the C bridge
cmd/wendy-core-wasm/     The same Go engine compiled for browsers
include/WendyCore/       Public C headers: binding API + reserved platform SPI
proto/                   Reserved structured Core schemas
internal/bridge/         Private native-loader mechanics
Package.swift            Local SwiftPM manifest of distributions/swift

distributions/
  javascript/            One ESM npm package: Node native + browser WASM
  python/                Python-version-independent native wheels
  swift/                 SwiftPM product WendyCore, statically linked
  kotlin/                Kotlin/JVM Maven artifact

bindings/                Binding patterns; SDKs live in their own repositories
platform/                Included adapter families/variants; Core-versioned
subsystems/              Reusable infrastructure behind explicit contracts
documentation/           Contract, projection, versioning and release guides
examples/                HelloWorld consumers in all six languages
scripts/                 Build, generation and release mechanics
../../.github/workflows/ Core CI (wendy-core-*); publication disabled
```

Language and platform are orthogonal. In particular, the Swift incarnation is not the Apple adapter, and the Kotlin/JVM incarnation is not an Android adapter.

## Agreed architecture

One SDK family serves Companion/CLI, WendyOS/Agent, and apps using Wendy capabilities. Selectable domain modules and their dependencies share one `WendyRuntime` foundation and one composed engine, rather than pulling every domain into each consumer.

Portable Core sits between a stable binding contract above and a platform-capability SPI below. Substantial infrastructure such as container execution has separate subsystem contracts so it can remain reusable even if Core were theoretically implemented in another language. Go remains the chosen implementation; alternative engines are not planned.

See [architecture and boundaries](documentation/architecture.md) for the diagrams and implementation-neutral contracts, and [module composition](documentation/modules.md) for dependency/scaffold rules. These are recorded design decisions, not features already implemented by the HelloWorld bootstrap.

## Try it

Clone the repository and enter the checkout:

```sh
git clone https://github.com/wendylabsinc/WendyOS.git
cd WendyOS/go/wendy-core
```

Prerequisites: Go, a C compiler, Python 3.9+, `protoc`, and Clang. JavaScript tests additionally need Node 22+; Kotlin needs JDK 21 and Gradle 9.8.1 on `PATH`. Swift tests need Swift 6.2+ and, on macOS, XcodeBuildMCP. Documentation generation needs Doxygen and Graphviz (`dot`).

```sh
make check               # real Go/C tests, generated JS interface, metadata
make javascript          # Node + actual browser, including installed npm artifact
make python              # Python calls the native engine
make swift               # Swift calls the statically linked engine (macOS also builds for iOS)
JAVA_HOME=/path/to/jdk21 make kotlin
make documentation       # Doxygen HTML/XML under .build/sdk/site
```

```go
import "github.com/wendylabsinc/WendyOS/go/wendy-core/core"
value := core.WendyHelloWorld() // 42; directly in Go
```

```js
import { WendyHelloWorld } from '@wendylabs/core';
console.log(WendyHelloWorld()); // 42; identical import/API in Node and browsers
```

The public JavaScript boundary is ESM with shared TypeScript declarations. Conditional exports select the backend. Node uses Node-API; the browser runs a real Go/WASM build. A CJS shim remains private to Node interop. The shared JS operation surface and declarations are generated from the C manifest; no Promise/iterator SDK sugar is included.

## HelloWorld examples

See [examples/README.md](examples/README.md) for small Go, C, Swift, Python, Kotlin/JVM and JavaScript applications. Each has its own run instructions and uses Wendy Core from disk; JavaScript includes both Node and browser execution.

## Bootstrap target matrix

| Target | C / Python / Node / Kotlin JVM | SwiftPM |
|---|---|---|
| Linux x64 (glibc 2.35+) | CI build/test target | CI build/test target |
| Linux arm64 (glibc 2.39+) | CI build/test target | CI build/test target |
| macOS x64 / arm64 (15+) | CI build/test targets | CI build/test targets |
| iOS 18+ / Simulator (arm64, x64) | — | CI build/link target (tests not run) |
| Windows x64 | CI build/test target | Not yet supported |
| Browser | Go/WASM, Chromium conformance | — |

[Wendy Core CI](https://github.com/wendylabsinc/WendyOS/actions/workflows/wendy-core-ci.yml) builds and tests this matrix. A passing run verifies the HelloWorld bootstrap, not the proposed domains or platform capabilities. Alpine/musl, Windows arm64, Android, watchOS and other variants are not claimed by this bootstrap; iOS is claimed for Swift only. Go callers use the native module directly; the native-binary matrix does not define every potential Go target.

## Release boundary

Tagged dependencies retain their release/prerelease version. Unreleased source builds report `0.1.0-dev+<7-character commit>` when known, or `0.1.0-dev`. Packaged engines retain their stamped build identity.

`VERSION` names the exact intended release, currently `0.1.0-seed.1`. This first SDK round is a SemVer prerelease: the nested Go module uses `go/wendy-core/v0.1.0-seed.1` tags. Remote Swift distribution from WendyOS is unresolved, and this migration creates no tags. CI derives immutable PR/manual-preview and nightly versions from it; packaged local builds use `-dev+<7-character HEAD commit>` when a checkout is available, or `-dev` otherwise. The runtime, selected modules, binding/platform/subsystem contracts and included adapters/backends belong to that release. All incarnations ship that version, not separate implementations or independent module/platform versions. Later SDKs depend on compatible Core releases and have independent release trains.

See [releasing](documentation/releasing.md) for packaging and migration boundaries. Passing PR/main builds produce versioned preview/nightly artifacts. Release builds are manual-only. Publication is disabled in workflow code and the publication CLI; repository variables, tag pushes and GitHub releases cannot enable it.

## License

Licensed under the Apache License, Version 2.0; see `LICENSE` and `NOTICE`. Native/WASM packages retain the Go runtime's separate BSD license notice. The Gradle wrapper is retained in the imported history but is not distributed at the tip.

## Contributing

Start with the architecture and module guides above. Keep shared behavior in Core, interop mechanical, and platform/subsystem implementations behind their contracts. Run `make check compare-clean documentation` and the affected language tests before submitting changes. Runtime/domain/provider placeholders are design scaffolding, not implemented APIs or support guarantees.

The C distribution is only the bridge foundation—not an ergonomic C SDK. Public headers plus protobuf schemas remain normative. API manifests, JS projections, and reference sites are derived artifacts. Planned object/stream patterns are design notes, not implemented APIs.
