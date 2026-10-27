# Buffers and protobuf values

Structured requests, responses, and ordinary stream items cross the bridge as versioned Wendy SDK protobuf bytes.

- `WendyDataView` is borrowed and copied only when the operation must retain it.
- `WendyData` is an immutable, owned output buffer.
- Bindings decode one response into their native generated protobuf value.
- The Go CLI bypasses this serialization boundary and uses Go values directly.

Large video, audio, file, and tunnel payloads use raw `WendyData` buffers with separate protobuf metadata. They are not embedded in another protobuf solely to cross the ABI.
