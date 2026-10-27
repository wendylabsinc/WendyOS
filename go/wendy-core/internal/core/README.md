# Private Go implementation mechanics

Reserved for implementation details shared by the direct Go incarnation and upper-boundary bridges, such as transport, routing, validation, retries and state machines. Public portable domain scaffolding lives in `core/`.

Private Go code may use Go-native types internally, but public binding, platform and subsystem contracts must not expose them. Keep concrete native/container infrastructure behind provider contracts rather than spreading its types throughout domain logic. Native platform adapters live in `platform/`; reusable infrastructure lives in `subsystems/`. Included implementations share the Core release version.

No product workflows are implemented yet. This directory is not a separate runtime, public SDK module, privileged Agent implementation or CLI presentation layer.
