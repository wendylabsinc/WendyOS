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

The first native operation, `WendyCloudListDevicesCreate`, copies a
`wendy.core.v1.CloudDeviceListRequest` and returns a dormant operation. The host
stores its handle before calling `WendyOperationStart`. This activation gate
ensures completion cannot precede the initiating Create's return; completion
may race Start's return. Start is idempotent and never waits for a host callback.
Dropping an unstarted operation discards it without execution or completion.
After Start, Core holds a reference until the completion has returned.

Completion transfers exactly one owned `WendyData` response or `WendyError`.
`WendyErrorGetCode` preserves numeric gRPC codes (1 for local cancellation,
4 for deadline), and `WendyErrorGetDetails` carries ordered/repeated binary
metadata as `wendy.core.v1.RPCErrorDetails`. Getters borrow immutable storage
until the object's final Release. Retain/Release require a live reference;
releasing the last reference concurrently with another use is invalid.

The device response wraps serialized Cloud v2 `Asset` messages from the existing
service contract, preserving optional and unknown fields. There is no second
device model or public offset API. Core constructs DPoP metadata through the
registered host's public key, secure entropy and exact-byte signing callbacks.
The caller binds that host's TLS transport to the supplied authority and
validates the operator's tenant/key/token before creating the operation.
