# Asynchronous unary operation

A unary operation accepts one protobuf request and completes exactly once with one protobuf response or one error.

| Projection | Shape |
|---|---|
| C bridge | `WendyOperation *` plus `WendyCompletion` |
| Full C SDK | Typed request and typed completion wrapper |
| Swift | `async throws -> Response` |
| Go | `(context.Context, *Request) (*Response, error)` |
| Kotlin | `suspend fun ...: Response` |
| Python | `async def ... -> Response` |

Cancelling the native task cancels the underlying `WendyOperation`. Releasing the operation without cancelling does not alter execution.
