# Language bindings

Each binding projects the normative C/protobuf contract into native naming, values, concurrency, errors, and resource lifetimes. Bindings may add ergonomic sugar but no independent product behavior.

Language SDK modules select compatible Core domains and share one runtime foundation. They must not load separate Go engines per module or hide a monolithic native payload behind apparently small wrappers. Platform adapters and subsystem backends are Core components, not language-binding implementations.

Core artifacts share the coordinated Core version; ergonomic SDKs have independent release trains in their own repositories, [wendy-swift](https://github.com/wendylabsinc/wendy-swift) and [wendy-go](https://github.com/wendylabsinc/wendy-go). The notes here record the intended binding patterns; current runnable interop is in `distributions/`.
