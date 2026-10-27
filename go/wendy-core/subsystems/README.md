# Reusable infrastructure subsystems

Reserved for coherent infrastructure implementations behind explicit contracts beside portable Core. These are part of the `wendy-core` source/release unit, not a generic public Services module.

The initial responsibility placeholder is `execution/`, for container execution and image management. No implementation, provider registration or IPC protocol is present.

Go Core may call a Go implementation directly in-process. C/protobuf IPC adapters are only needed at real language/process boundaries. A theoretical Rust/Swift/C++ Core must be able to retain a Go-powered infrastructure implementation rather than recreate its ecosystem.

Keep concrete infrastructure types out of portable domain APIs. Contract data, ownership, errors, cancellation and lifecycle must be implementation-neutral. Preserve authoritative checks in the responsible service/Agent. Do not introduce a new daemon, plugin framework, or an abstraction around every Go utility merely to populate the scaffold.
