# Wendy Core

`wendy-core` is one versioned release unit containing the portable engine, selected modules, public contracts and included platform/subsystem implementations. Go is the chosen implementation; incarnations are released in several dependency-manager formats. Townhall is the historical repository codename.

## Current operation

`WendyHelloWorld()` returns the signed 32-bit integer `42`, synchronously without transferring owned resources or requiring a platform capability. It is implemented once in Go and projected into C, Swift, Python, Kotlin/JVM and JavaScript.

`WendyCoreVersion()` returns the effective engine version as a borrowed process-lifetime UTF-8 C string, copied into language strings by projections. Go exposes the same query as `core.Version()`. Unreleased source builds use `0.1.0-dev+<7-character commit>` when known; tagged dependencies retain their release/prerelease version and packaged engines retain their stamped build identity.

JavaScript has one ESM package, one operation interface, shared TypeScript declarations, and separate Node/native and browser/WASM backends. Both execute the same conformance suite against the actual engine.

## Contract and layering

The canonical contract is in `include/WendyCore` and `proto/wendy/core/v1`. Language-incarnation packages perform interop only. Future Wendy language SDKs add ergonomics above Core; platform adapters provide native capabilities behind a hard boundary and share the Core release version.

Object ownership, async unary, demand-driven streams and bulk buffers under `binding-patterns` and `c-contract` are planned conventions. They do not add operations to the current release.

The agreed modular design shares one `WendyRuntime` foundation across selected modules. Portable Core has hard binding and platform boundaries, plus explicit contracts for reusable infrastructure backends. This is a design direction, not implemented module/provider APIs.

See [architecture](architecture.md), [modules](modules.md), [API manifest](api-manifest.md), and [releasing](releasing.md) for the diagrams, contracts and build workflow.
