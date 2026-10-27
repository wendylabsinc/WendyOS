# Borrowed string unary

A synchronous query returns a non-null, NUL-terminated UTF-8 string. The operation documents its lifetime. Callers must not modify or free the borrowed storage. No Go pointer crosses the C ABI.

Language projections copy the result into their ordinary string type (Swift `String`, JavaScript `string`, Python `str`, Kotlin `String`). The operation is synchronous in all projections and transfers no resources.
