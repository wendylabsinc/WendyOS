# Module composition

Wendy Core is one strict-SemVer release unit with selectable modules and an explicit dependency graph. Platform adapters and included subsystem implementations share that version; ergonomic language SDKs may have independent versions. Modular consumption does not require an independent release train for every namespace.

## Current scaffold

| Directory | Status |
|---|---|
| `core/` | Direct Go HelloWorld implementation |
| `core/runtime/` | Shared-runtime responsibility placeholder |
| `core/identity/` | Shared sign-in, refresh, session/context and organization protocols |
| `core/devices/` | Shared connection and device-identity policy |
| `core/enrollment/` | Shared enrollment, Cloud removal and reset-proof policy |
| `core/applications/` | Proposed application-management domain, no APIs |
| `core/events/` | Proposed event domain, no APIs |
| `platform/` | Platform-family/variant responsibility placeholders |
| `subsystems/execution/` | Execution/image infrastructure placeholder |

The placeholder directories are not Go packages and do not claim implemented functionality or target support. Runtime objects and module selection are not implemented by the scalar demo.

## Dependency rules

- Selected domains reference one shared runtime foundation and one composed engine.
- Include only selected modules and their actual transitive dependencies.
- Keep the foundation free of optional domain implementations; avoid dependency cycles.
- Do not force Identity, discovery or deployment into every consumer.
- A possible Applications → Devices dependency is illustrative, not a finalized graph. Establish concrete edges with actual implementations, rather than a speculative machine-readable manifest.
- Native capability implementations stay in `platform/`; concrete container/image machinery stays behind `subsystems/` contracts. Portable domain APIs do not expose those concrete types.
- Client APIs and provider implementations share domain contracts, not privileges or all implementation dependencies.

## Artifact composition

Small SDK wrappers around one large prebuilt native/WASM payload do not achieve selective consumption. Future build composition must exclude unselected domain implementations and unnecessary adapters/backends from the final artifact. Compile one composed Go engine rather than a Go shared library/runtime for every module.

Standard prebuilt bundles may be useful for package managers, but their selection is not settled. Measure final native/WASM sizes: the Go runtime has a baseline cost and wrapper tree-shaking cannot remove an already-built native library. The bootstrap still builds the single HelloWorld engine in all incarnation formats; this page does not introduce a module build system.

Concrete additional domains (Configuration, Secrets, Messaging, Media or Cloud Management) should be added only when real Wendy APIs justify them, not as a generic Services bucket.

See [architecture](architecture.md) for consumer roles, both hard boundaries and reusable subsystem contracts, and [releasing](releasing.md) for version/distribution rules.
