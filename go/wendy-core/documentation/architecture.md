# Architecture and names

This page preserves the agreed direction. It is not a claim that the modules, runtime objects or provider interfaces below are implemented. The current bootstrap exports `WendyHelloWorld()` and `WendyCoreVersion()`, implemented in Go, and provides local examples in all six initial languages.

## Names and release boundaries

- **`wendy-core`**: canonical repository and coordinated Core release unit. Townhall is the historical codename.
- **WendyOS**: operating system and device-image integration, distinct from the reusable Core. This scaffold does not rename or migrate the existing OS/Agent repositories.
- **Wendy Core**: portable engine and its contracts, independently released in language-consumable incarnations. The implementation is Go; implementation-language independence is a design constraint, not a plan to build alternative engines.
- **Wendy Platform**: native capabilities, implemented in the platform's appropriate language, inside `wendy-core/platform/`. These adapters use the Core version, not an independent platform release train.
- **Wendy Swift / Wendy Python / Wendy Kotlin / Wendy JavaScript / Wendy Go / Wendy C**: language SDKs above Core, collectively the **Wendy SDK**. Each language has two layers: a thin Core distribution in this repository (for example the Go `core` package or the Swift `WendyCore` product), and an ergonomic SDK in its own repository that depends on it ([wendy-go](https://github.com/wendylabsinc/wendy-go), [wendy-swift](https://github.com/wendylabsinc/wendy-swift)). Those SDKs are scaffolds so far; the full C SDK remains deferred.
- **WendyRuntime**: shared runtime foundation used by the selected modules, not a separate engine instantiated by each module.

Spaced names are used in prose; joined names such as `WendyCore` are used where a code/module identifier is needed. Repository organizational directories are lowercase; language conventions such as SwiftPM `Sources` and public C `WendyCore` are retained.

One strict-SemVer Core release covers the shared runtime, selected modules, binding/platform/subsystem contracts and included adapters/backends. There is no independent module or platform version stream. One Core version has C, Go, SwiftPM, Python, Maven and npm incarnations. These packages contain necessary interop, not independent product implementations. Modular Core artifacts remain part of one coordinated Core SemVer release. SDKs can independently version their ergonomic layers and declare compatible Core dependencies. See [releasing](releasing.md) for current distribution mechanics and compatibility obligations.

## One SDK family, three consumer roles

The SDK serves a unified Wendy system rather than three independently implemented SDKs:

| Consumer role | Responsibility |
|---|---|
| Companion: CLI and desktop/mobile apps | Discover, connect, deploy, control and observe |
| WendyOS / Agent | Provide operations, manage device resources and enforce authority |
| Apps using Wendy capabilities | Consume permitted capabilities and participate in application/device interactions |

Divide public APIs by domain, not by role. Keep common contracts, types, errors and streaming semantics consistent. Importing a module grants no privileges: the relevant credentials, entitlements and authoritative authorization checks still determine access.

Providing an operation is different from consuming it. The Agent should not call itself through a client SDK to execute a container operation. It can reuse shared Core machinery while implementing the operation directly. Provider contracts belong alongside their domain; privileged Agent internals do not become public client APIs or dependencies of every browser/mobile app.

## Optional modules and their dependency graph

Consumers should select modules and their transitive dependencies, without paying for unrelated domains. The following names and graph are illustrative, not a finalized API inventory or dependency specification:

```text
Runtime / common contracts
├── Identity
├── Devices
│   └── Applications
└── Events
```

| Proposed domain | Intended responsibility |
|---|---|
| Runtime | Configuration of the runtime, sessions, common errors, cancellation, buffers and lifecycle primitives |
| Identity | Client authentication, pairing, credentials and trust; platform-backed keys where applicable, not backend CA implementation |
| Devices | Discovery, connections, device information, capabilities and device configuration |
| Applications | Deployment, installation and lifecycle management |
| Events | Publishing and subscribing to structured events |

The generic **Services** bucket was removed from the proposal because it was too vague. Add concrete domains such as app-scoped **Configuration**, **Secrets**, **Messaging**, **Media** or **Cloud Management** only when actual Wendy APIs justify them. These names are candidates, not existing service/support claims. App-scoped configuration is distinct from configuring the runtime or a device.

Dependency edges follow implementation needs. Do not force an app reading configuration to depend on discovery/deployment, or make every domain depend on Identity. Local versus Cloud is usually routing beneath a domain operation, not a duplicate domain API. Neither the consumer's language nor the adapter's platform determines the domain split.

### Shared runtime, selective engine composition

```text
Application
└── Shared WendyRuntime
    ├── Selected module
    ├── Selected module
    └── Their required dependencies
```

- Selected modules reference the same runtime rather than creating independent engines.
- Common infrastructure and the Go runtime are included once in a composed native/WASM engine, not once per module.
- One shared runtime foundation does not require a global singleton. Isolated logical runtime instances can share the same engine code.
- Physical SDK module dependencies must correspond to real Core implementation/build boundaries. Small language packages that all load a monolithic Core do not solve binary size.
- Build composition includes only the selected domains and their required dependencies. Standard prebuilt bundles may be useful for package-manager distribution; their selection is not settled.
- Avoid one separately compiled Go shared library per domain: that can duplicate Go runtimes/common code and complicate shared ownership and handles.
- Measure the final native/WASM payload, not just the import graph. Wrapper tree-shaking does not reliably remove an already-built native library or exported WASM functionality. The Go runtime also imposes a baseline size.

The [module scaffold](modules.md) reserves the runtime/domain directories and documents composition rules; they contain responsibility notes, not module APIs or Go packages. The current bootstrap has a single-operation engine, not selective module builds. No new package/repository/version stream is required merely to record a logical module. Physical separation should serve dependency and size requirements, not create machinery for its own sake.

## Two hard boundaries around portable Core

```text
           CLI / Companion / Agent / Apps
                           │
                           ▼
 ┌─────────────────────────────────────────────────┐
 │ Language bindings / SDK modules                 │
 │ Swift · Python · Kotlin · JavaScript · …         │
 └─────────────────────────────────────────────────┘
                           ↕
 ═══════════════════ UPPER BOUNDARY ═════════════════
          Binding interface / Core contract
                   C ABI + protobuf
 ═══════════════════════════════════════════════════
                           ↕
 ┌─────────────────────────────────────────────────┐
 │                  Portable Core                  │
 │                                                 │
 │        Shared WendyRuntime foundation           │
 │        Selected modules + dependencies          │
 │                                                 │
 │        Shared product behavior, independent      │
 │        of consumer language and platform        │
 └─────────────────────────────────────────────────┘
                           ↕
 ═══════════════════ LOWER BOUNDARY ═════════════════
             Platform capability interface
                         SPI
 ═══════════════════════════════════════════════════
                           ↕
 ┌─────────────────────────────────────────────────┐
 │ Platform adapters                               │
 │ Apple · Android · Linux · Windows · Web          │
 │                                                 │
 │ Native capabilities and implementations         │
 └─────────────────────────────────────────────────┘
```

### Upper boundary: bindings

Public C headers, public protobuf schemas and their documented semantic contract are normative. Generated manifests, projections and reference sites are derived, not alternate definitions of behavior.

The C bridge optimizes for minimal crossings, typed entry points, opaque lifecycle objects, explicit cancellation and demand-driven streams. Structured control values use protobuf; bulk video/audio/file/tunnel payloads use immutable raw buffers. Public opaque C types use PascalCase and explicit pointers, without `_t` or `Ref` suffixes. Inputs/local object references use east-const; returned/callback output objects and output parameters remain non-const. Do not expose Go pointers or native Go concurrency types across the boundary.

Language SDKs add idiomatic ergonomics, not independent validation, routing, authorization, retries, state machines or product semantics. Shared behavior currently lives in Go Core. Go consumers import that implementation directly, without a Go → C → Go round trip; Go APIs are a stable public projection, not the language-neutral specification.

JavaScript has one npm package, a shared public ESM interface and TypeScript declarations. Node/native and browser/Go-WASM backends project the same contract. Native handles must map to runtime-independent opaque representations, not exposed pointers. Ownership, cancellation and demand semantics must remain equal across backends. The scalar demo does not implement those future lifecycle APIs.

### Lower boundary: native capabilities

Public C/C++ headers use the conventional `include/` root. `include/WendyCore/WendyCore.h` declares the upper API. `include/WendyCore/WendyPlatform.h` declares the lower host-provider contract for signing and serialized RPC transport, including registration, cancellation and demand-driven streams. Private headers remain beside implementation code. Both public boundaries are covered by Core compatibility guarantees.

Platform adapters are included components of the `wendy-core` release; living in the same repository does not collapse their boundary. Builds include only the necessary adapters/capabilities. Platform adapters expose narrow capabilities rather than product workflows. Examples include hardware-backed keys, native discovery, lifecycle/background execution and native I/O. An Apple adapter may be Swift while serving Python, Kotlin or JavaScript consumers; adapter implementation language is independent of the binding language.

Distinguish unsupported capability, temporary unavailability and permission denial. A uniform interface must not imply identical capability availability or silently change product behavior across bindings/platform variants.

## Subsystem boundaries beside Core

The `subsystems/` scaffold reserves reusable infrastructure implementations and an `execution/` responsibility placeholder. Included implementations share the Core version; these directories do not introduce an independent release or implemented backend. Keep substantial infrastructure reusable independently of the Core implementation. Distinguish platform primitives from coherent subsystem backends such as container execution and image management:

```text
                Language SDKs
                      ↕
            Stable binding contract
                      ↕
            ┌──────────────────┐
            │  Portable Core   │
            │  WendyRuntime    │
            │  Domain modules  │
            └──────────────────┘
                 ↕          ↔ Stable subsystem contracts
          Platform SPI                    ↕
                 ↕             ┌────────────────────────┐
         Platform adapters     │ Infrastructure         │
                               │ implementations        │
                               │ Go / containerd / …    │
                               └────────────────────────┘
```

An alternative Rust, Swift or C++ Core could satisfy the same binding/platform contracts while continuing to use an existing Go-powered container backend. It should not have to recreate the Go ecosystem to replace portable Core logic. Alternative implementations could theoretically coexist where composition and ownership permit; none is planned or authorized by this design.

Core understands required operations, results and product workflows; a subsystem implementation owns its concrete execution and subsystem-internal machinery. Preserve authoritative checks at the responsible service/Agent boundary. Do not distribute client-side replicas of privileged implementation code.

These are logical boundaries, not a mandate for new processes or a generic service/plugin framework. Initially, Go Core can call a Go backend directly in-process. A C ABI or protobuf-based IPC adapter can expose the same contract when a language/process boundary actually requires it. Abstract a coherent subsystem worth retaining, not every utility or library merely because it happens to be Go.

## Implementation-neutral contracts and conformance

Both Core boundaries, and reusable subsystem contracts, must define behavior rather than reveal Go implementation details:

- Use implementation-neutral data representations and explicit ownership; no Go pointers, channels, interfaces or runtime objects crossing them.
- Specify errors, callbacks/threading, cancellation, streaming demand, resource lifecycle and capability availability, not signatures alone.
- Keep concrete infrastructure types out of portable domain logic through explicit dependency contracts.
- Use independent conformance tests to demonstrate equivalent observable behavior across projections/backends and any theoretical alternative implementations.
- Version public boundaries and supported-target guarantees under strict SemVer. Do not claim the current signature comparator proves complete ABI, wire or behavioral compatibility.

The contract need not be reimplemented in another language to establish these boundaries. Go remains the chosen shared implementation, and the actual native/browser conformance proof currently covers the demo operation only.

## Independent axes and current scope

Language bindings, platform families and platform variants are separate axes. Swift can target Linux/Windows in principle; Kotlin/JVM consumption is not Android support. A language running on a target does not prove that Core or its capabilities run there. Each release declares supported combinations and system minima. Apple has shared-family and variant directories for iOS, macOS and watchOS; these remain placeholders, not support claims.

The [bootstrap matrix](../README.md#bootstrap-target-matrix), [local examples](../examples/README.md) and [release guide](releasing.md) describe what exists today. Preserving this architecture does not implement modules, change current APIs, start product migration, introduce an alternative Core, or authorize publication. Existing authorization and security requirements remain in force; Cloud/Auth/PKI consumer requirements apply at affected boundaries, not as wholesale backend obligations for every SDK module.
