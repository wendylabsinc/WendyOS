# Demand-driven stream

A stream accepts one protobuf request, emits ordered protobuf items while demand is available, and completes once.

| Projection | Shape |
|---|---|
| C bridge | `WendyStream`, callbacks, and explicit demand |
| Full C SDK | Typed item callback over the same stream |
| Swift | `AsyncThrowingSequence<Item, Error>` |
| Go | `Stream[Item].Next(context.Context)` |
| Kotlin | `Flow<Item>` |
| Python | `AsyncIterator[Item]` |

A binding must not request unbounded demand merely to emulate its language abstraction.
