# Portable Wendy Core

The chosen implementation is Go. The binding contract above and platform/subsystem contracts below/beside it remain implementation-neutral; no alternative Core implementation is planned.

`core.go` provides the `WendyHelloWorld()` bootstrap. Native and browser bridges call that same implementation. Working domain APIs live in the packages below; they are not limited to the hello bootstrap. Go consumers call Core directly without a Go → C → Go round trip.

## Domain packages

- `runtime/`: proposed shared `WendyRuntime` foundation.
- `identity/`: shared sign-in, session and organization-selection protocols.
- `cloud/`: Cloud requests and inventory, plus the basic Cloud v2 notification inbox in `notifications.go` (get, stable pages, count, delete and read status).
- `devices/`: shared device connection and Agent RPC behavior.
- `enrollment/`: shared enrollment, removal and reset-evidence policy.
- `lite/`: shared Wendy Lite setup, enrollment, state, GPIO, media and transport behavior.
- `applications/`: proposed application-management domain.
- `events/`: proposed structured-event domain.

Identity, Cloud, Devices, Enrollment and Lite contain working Go APIs. Runtime, Applications and Events still contain responsibility notes. Exact dependency edges are not settled; see [modules](../documentation/modules.md). Selected modules must share one composed engine and include only their required dependencies, not a separate Go runtime per module.

Portable domain code uses contracts for native capabilities and substantial infrastructure, not concrete platform/container types. Platform adapters live in `platform/`; reusable infrastructure lives in `subsystems/`. Both are components of the same versioned `wendy-core` release.
