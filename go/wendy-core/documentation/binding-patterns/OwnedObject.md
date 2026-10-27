# Owned object

An owned C object is opaque and reference-counted. Bindings preserve deterministic cleanup without exposing retain counts.

| Projection | Shape |
|---|---|
| C bridge | `Create`, `Retain`, and `Release` |
| Full C SDK | Same object, optionally wrapped by typed conveniences |
| Swift | Final class releasing in `deinit` |
| Go | Pointer with explicit `Close` where resources outlive a call |
| Kotlin | `AutoCloseable` object |
| Python | Context-managed object with deterministic `close` |

Garbage collection may be a safety net, but it is not the primary lifetime for connections, streams, or other scarce resources.
