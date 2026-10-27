# Errors

`WendyError` combines a stable `WendyErrorCode`, a human-readable UTF-8 message, and eventually a versioned protobuf details value.

Language bindings map the same category into their native error mechanism. Cancellation and deadlines preserve native behavior where possible:

| C | Go | Swift | Kotlin | Python |
|---|---|---|---|---|
| `WendyErrorCancelled` | `context.Canceled` | `CancellationError` | coroutine cancellation | task cancellation |
| `WendyErrorTimeout` | `context.DeadlineExceeded` | `WendyError.timeout` | timeout exception | timeout exception |

Messages are diagnostic and are not stable programmatic identifiers.
