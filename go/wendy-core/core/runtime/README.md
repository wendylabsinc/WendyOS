# Runtime foundation

Reserved for the shared `WendyRuntime`: runtime configuration, sessions, common errors, cancellation, buffers and resource lifecycles. It is included once across selected modules and their dependencies, not instantiated as a separate engine by each domain.

This does not require a global singleton. Isolated logical runtimes can share the same compiled engine. No runtime-object API is implemented yet; the scalar HelloWorld entry point remains in `core/core.go`.
