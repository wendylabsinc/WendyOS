# Swift binding

The Swift SDK lives in [wendy-swift](https://github.com/wendylabsinc/wendy-swift), on top of this repository's `WendyCore` product.

The Swift projection will wrap owned bridge objects in final classes, expose unary operations with `async throws`, and expose demand-driven streams as `AsyncThrowingSequence`. Generated protobuf values provide structured models.
