// Package history is the durable record of how the spatial state changed, and
// reconstructs that state as it was at any retained instant.
//
// A Journal is an append-only log of world.Component deltas (upserts and
// tombstones) in one directory. Nothing in it is estimated: a reconstruction
// is exact, or it is refused with a typed error (ErrCompacted before the
// retention horizon, ErrCorrupt when the stored bytes cannot be trusted).
//
// # Two clocks
//
// Every record has two times, and every query names the one it means:
//
//   - SampleTime is Timing.SampleTime, when the publisher says the state
//     held. "Where was the rover at 10:32" is a SampleTime question. Sample
//     times may arrive in any order.
//   - ApplyTime is the injected clock at Append, when this node learned the
//     record. "What did the node believe at 10:32" is an ApplyTime question,
//     and its answer is what a world.Replica fed the same records under the
//     same clock returned from Snapshot at that instant.
//
// On either axis the state of a key at T is its record of the highest
// world.Version among those timed at or before T. A tombstone at that version
// removes the key; a lower version never resurfaces, late or not. With
// View.HonourTTL the record also has to be alive at T: before SampleTime + TTL
// on the sample axis (spatial.Entity.ValidUntil), before the expiry
// world.Replica computes (TTL less the sample's age, error bound and age hint
// at application) on the apply axis.
//
// Components are returned in the canonical JSON form world.Replica keeps
// (compact payload, no monotonic clock reading), in the key order of
// Replica.Snapshot. Frame visibility is the reader's concern: filter on
// Component.Frame.
//
// # Storage
//
// Records are framed with a length, a checksum of the length and a CRC-32C of
// the body, in segment files "seg-<first sequence>.seg". A final frame cut
// short by a crash is truncated on Open (Stats.TruncatedBytes); a bad frame
// anywhere else, a missing segment or a sequence gap fails Open with
// ErrCorrupt rather than dropping what follows. Every read verifies its frame
// again. Append leaves durability to the caller's cadence: records reach
// stable storage at Sync, at Close and whenever a segment is sealed.
//
// Compact collapses the history before a horizon into a snapshot file
// "snap-<generation>.snap" holding only the records a query at or after
// the horizon can still return, and deletes the segments it replaces. Queries
// at or after the horizon answer exactly as before; queries before it return
// ErrCompacted. With Config.Retention set this happens by itself each time a
// segment fills, which is the journal's periodic snapshot. Without it the
// whole history is kept and no snapshot is written: the in-memory index
// answers as-of queries directly, so a snapshot would buy nothing.
//
// A Journal is safe for one appender and any number of concurrent readers in
// one process. It publishes no components of its own.
package history
