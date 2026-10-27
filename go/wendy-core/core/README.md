# Portable Wendy Core

The chosen implementation is Go. The binding contract above and platform/subsystem contracts below/beside it remain implementation-neutral; no alternative Core implementation is planned.

`core.go` is the current direct Go entry point and implements only `WendyHelloWorld()`. Native and browser bridges call this same implementation. Go consumers never take a Go → C → Go round trip.

## Module scaffolding

- `runtime/`: the shared `WendyRuntime` foundation, included once.
- `identity/`: proposed identity domain.
- `devices/`: proposed device domain.
- `applications/`: proposed application-management domain.
- `events/`: proposed structured-event domain.

These directories contain responsibility notes, not Go packages or implemented APIs. Exact dependency edges are not settled; see [modules](../documentation/modules.md). Selected modules must share one composed engine and include only their required dependencies, not a separate Go runtime per module.

Portable domain code uses contracts for native capabilities and substantial infrastructure, not concrete platform/container types. Platform adapters live in `platform/`; reusable infrastructure lives in `subsystems/`. Both are components of the same versioned `wendy-core` release.
