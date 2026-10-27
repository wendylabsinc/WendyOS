# Bulk data

Bulk payloads cross the bridge as immutable owned buffers rather than nested protobuf bytes.

| Projection | Preferred shape |
|---|---|
| C bridge | `WendyData *` plus protobuf metadata |
| Full C SDK | Borrowed byte view scoped to the owning data object |
| Swift | `Data` or a scoped no-copy byte view |
| Go | `[]byte` or an `io.Reader`-style stream |
| Kotlin | `ByteBuffer` |
| Python | `memoryview` |

Bindings must make borrowed lifetimes explicit. A no-copy view must never outlive its owning `WendyData`.
