# Streaming

C streams are ordered and demand-driven.

- A new stream starts with zero demand.
- `WendyStreamRequest` adds demand without blocking.
- Each delivered item consumes one unit of demand.
- Item callbacks for one stream are serialized.
- Completion occurs exactly once after the final item callback returns.
- Cancellation is explicit and idempotent.

Bindings request more demand only as their native consumer advances. This maps to `AsyncSequence`, Kotlin `Flow`, Python `AsyncIterator`, and Go's pull-based `Next` without introducing an unbounded intermediate queue.
