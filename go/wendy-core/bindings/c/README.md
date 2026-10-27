# Full C SDK

The optional human-facing Wendy C SDK will be a peer binding over `include/WendyCore`, not the binary waist itself. It is deferred; the current C distribution is only Wendy Core's raw bridge. It may materialize protobuf messages into generated C values and add typed conveniences while preserving bridge ownership, cancellation, and streaming semantics.
