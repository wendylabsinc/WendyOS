package services

import (
	"sync"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The two-plane camera path, and why the frame identity binding is shaped the
// way it is.
//
// An application that wants to use ordinary tooling (ffmpeg, GStreamer) should
// not need a bespoke gRPC integration to read frames, and a raw device node
// makes the app an INDEPENDENT second reader of the camera, so the frame a
// model scores is not provably the frame the episode recorded. The two-plane
// path fixes both: one producer, two planes.
//
//   - Data plane: a v4l2loopback node fed from the SAME producer hub that
//     episode capture and gRPC subscribers already consume. The app reads that
//     node with any standard tool, unmodified.
//   - Control plane: identity (source id, sample id, canonical boottime,
//     uncertainty) rides in-band, in the buffer timestamp the agent writes when
//     it feeds the node, so it needs no channel of its own.
//
// The binding between them is this file's subject.
//
// # Why sample_id cannot be written into v4l2_buffer.sequence
//
// The obvious design is to stamp our sample_id into v4l2_buffer.sequence on the
// buffer we queue, so a consumer reading the loopback reads our identifier back
// out directly. THAT DOES NOT WORK, and the reason is in the v4l2loopback
// module rather than in anything we control.
//
// In v4l2loopback's VIDIOC_QBUF handler for V4L2_BUF_TYPE_VIDEO_OUTPUT, the
// sequence field of the queued buffer is overwritten unconditionally:
//
//	bufd->buffer.sequence = dev->write_position;
//
// (and identically in the write() path). dev->write_position is the module's
// own monotonic count of frames written to that node. Whatever sequence value a
// writer supplies is discarded. There is no module option, format flag, or
// ioctl that disables this overwrite OF THE SEQUENCE. So a design that says
// "sequence IS the sample id" would be claiming a guarantee the kernel actively
// prevents us from keeping.
//
// # The timestamp, unlike the sequence, IS the writer's
//
// That limit is specific to the sequence field, and it would be wrong to read
// it as "the writer controls nothing per buffer". Verified against v4l2loopback
// v0.15.4, the version the Yocto recipe pins, the very same OUTPUT branch of
// vidioc_qbuf hands the writer's timestamp straight through:
//
//	if (!(bufd->buffer.flags & V4L2_BUF_FLAG_TIMESTAMP_COPY) &&
//	    (buf->timestamp.tv_sec == 0 && buf->timestamp.tv_usec == 0)) {
//	        v4l2l_get_timestamp(&bufd->buffer);
//	} else {
//	        bufd->buffer.timestamp = buf->timestamp;
//	        bufd->buffer.flags |= V4L2_BUF_FLAG_TIMESTAMP_COPY;
//	        bufd->buffer.flags &= ~V4L2_BUF_FLAG_TIMESTAMP_MONOTONIC;
//	}
//
// The module substitutes its own clock only for a writer that supplies a ZERO
// timestamp. A nonzero one is copied verbatim and latches
// V4L2_BUF_FLAG_TIMESTAMP_COPY, which says exactly "this value came from the
// writer". On the read side vidioc_dqbuf's CAPTURE branch does
// `*buf = dev->buffers[index].buffer;` and then unset_flags, which clears only
// V4L2_BUF_FLAG_QUEUED and V4L2_BUF_FLAG_DONE, so both the timestamp and the
// COPY flag reach the reading application intact.
//
// So identity does ride in-band after all, just not in the field the naive
// design wanted. The pump stamps each buffer with the frame's canonical
// CLOCK_BOOTTIME receipt truncated to whole microseconds by struct timeval,
// which is the same number FrameIdentity.boottime_nanos carries divided by
// 1000; the truncation is exact because 1e9 divides evenly by 1000. A consumer
// can therefore check every frame it dequeues against the identity it resolved,
// with no extra field on the wire and no side channel needed for the check.
//
// The sequence remains the PRIMARY join key and the timestamp is a secondary
// one. Two reasons: the sequence is what makes an application-side drop visible
// (see drop case 2 below), which a timestamp cannot do, and the sequence is
// exact where microseconds are truncated. Nothing below changes.
//
// # What is sound instead: observe the kernel's number, publish the mapping
//
// The same QBUF handler copies the buffer struct back out to userspace after
// stamping it, and does so BEFORE the write position is advanced:
//
//	bufd->buffer.sequence = dev->write_position;
//	set_queued(bufd->buffer.flags);
//	*buf = bufd->buffer;          // <- returned to the writer
//	buffer_written(dev, bufd);    // <- this is what does ++dev->write_position
//
// So the writer LEARNS, from the ioctl it just issued, the exact sequence the
// kernel assigned to the frame it just wrote. That value is authoritative, it
// requires no assumption that the counter started at zero (it does not reset
// per open, only at device creation), and it does not depend on arrival order
// or on counting frames ourselves.
//
// The binding is therefore inverted relative to the naive design: the pump does
// not choose the sequence, it observes the one the kernel chose, and records
// (loopback sequence -> sample identity) in the table below. The control plane
// publishes that mapping. An app reading the loopback takes the sequence off the
// buffer it dequeued and looks it up.
//
// # What a consumer sees when frames are dropped
//
// There are two independent drop points, and they are detected by two DIFFERENT
// signals. Conflating them is the main way a consumer of this API can fool
// itself, so both are spelled out here and both are represented in the record.
//
//  1. Hub -> pump. The hub drops frames for a subscriber that is not reading
//     fast enough (deviceHub.broadcast's non-blocking send, counted per
//     subscriber in subDrops). Those frames are never written to the loopback at
//     all. The loopback sequence therefore stays DENSE across such a drop: the
//     kernel counts only what we actually wrote, so a consumer watching only
//     v4l2_buffer.sequence sees no gap and would wrongly conclude nothing was
//     lost. What does change is sample_id, which jumps. This is why every
//     binding carries HubDropsBefore, and why the control plane reports the
//     sample_id: a gap here is visible ONLY on the control plane.
//
//  2. Pump -> app. The application reading the loopback node is not keeping up,
//     and the module's ring overruns. v4l2loopback handles this in
//     get_capture_buffer by fast-forwarding the reader rather than blocking:
//
//     if (dev->write_position > opener->read_position + dev->used_buffer_count)
//     opener->read_position = dev->write_position - 1;
//
//     The reader's next dequeued buffer therefore carries a sequence that has
//     JUMPED. Frames the pump genuinely wrote never reached this reader. This
//     gap IS visible on the data plane: consecutive dequeued buffers differ in
//     sequence by more than one, and the difference minus one is exactly the
//     number of frames that reader missed. sequenceGap below computes it.
//
// Stated as a rule for consumers: a jump in sample_id with dense sequence means
// the agent could not keep up feeding the node; a jump in sequence means the
// application could not keep up reading it. Both can happen at once, and they
// are additive, not alternatives.
//
// A third case is not a drop and must not be reported as one: if the pump fails
// to write a frame, it records no binding and the kernel never advances its
// counter, so the frame is simply absent from both planes rather than appearing
// as a lost one.

// loopbackBinding is one recorded correspondence between a frame the pump wrote
// to a v4l2loopback node and the harness identity of the sample it came from.
//
// LoopbackSequence is the value the KERNEL assigned and handed back on QBUF, not
// a value we chose (see the file comment). Everything else is copied from the
// hub frame, so a consumer that resolves a sequence gets exactly the identity
// the episode index and the model-input ledger recorded for the same sample.
type loopbackBinding struct {
	// LoopbackSequence is the kernel-assigned v4l2_buffer.sequence, as read back
	// from the VIDIOC_QBUF that queued this frame on the node's OUTPUT queue.
	LoopbackSequence uint32
	// SampleID is the producer hub's identity for this sample within its source.
	// It is the join key to the episode capture index and the model-input ledger.
	SampleID uint64
	// BootNanos is the agent's bracketed CLOCK_BOOTTIME receipt of the sample and
	// UncertaintyNanos the bracket half-width, both copied verbatim from the hub
	// frame so every plane reports the same canonical time for the same sample.
	BootNanos        int64
	UncertaintyNanos int64
	// HubDropsBefore counts samples the hub dropped for the pump since the
	// previously written frame. It is the ONLY signal for case 1 in the file
	// comment: those samples never reached the node, so the loopback sequence
	// does not gap and cannot report them.
	HubDropsBefore uint64
}

// loopbackBindingRetention is how many recent bindings the table keeps.
//
// Sized for the join to succeed, not for history: an application reads the
// loopback and the control plane concurrently, so it resolves a sequence within
// a few frames of the pump recording it. A few seconds of slack at ordinary
// frame rates is ample, and the bound is what stops a long-running pump from
// growing without limit. Lookups older than this fail closed (found=false)
// rather than returning a guess.
const loopbackBindingRetention = 256

// loopbackBindingTable records recent (loopback sequence -> sample identity)
// bindings for one node and resolves lookups against them.
//
// It is a bounded ring with a sequence index rather than a plain slice offset
// calculation. The arithmetic shortcut would be to assume sequences are dense
// and index by difference from the newest, but that assumes we are the only
// writer on the node; dev->write_position advances for ANY writer. Indexing
// explicitly costs one map entry per frame and stays correct if that assumption
// is ever violated, which matters because the failure mode of the shortcut is
// silently resolving a sequence to the wrong sample: exactly the
// misattribution this whole path exists to prevent.
//
// Safe for concurrent use: the pump records, the control plane looks up.
type loopbackBindingTable struct {
	mu sync.Mutex
	// ring holds up to len(ring) bindings, oldest evicted first.
	ring []loopbackBinding
	// index maps a loopback sequence to its position in ring. Entries are removed
	// when the slot they point at is overwritten.
	index map[uint32]int
	// next is the ring slot the next recorded binding will occupy.
	next int
	// count is how many slots are populated, saturating at len(ring).
	count int
	// haveNewest reports whether newest holds a real sequence yet, so that a
	// genuine sequence of 0 is not mistaken for "nothing recorded".
	haveNewest bool
	newest     uint32
}

// newLoopbackBindingTable returns a table retaining the most recent retain
// bindings. A retain of zero or less uses loopbackBindingRetention.
func newLoopbackBindingTable(retain int) *loopbackBindingTable {
	if retain <= 0 {
		retain = loopbackBindingRetention
	}
	return &loopbackBindingTable{
		ring:  make([]loopbackBinding, retain),
		index: make(map[uint32]int, retain),
	}
}

// Record stores one binding, evicting the oldest if the table is full.
//
// Recording the same sequence twice replaces the older entry's index mapping, so
// a lookup always resolves to the most recently recorded sample for that
// sequence. That only arises if the kernel counter wraps a full 2^32 within the
// retention window, which cannot happen at any real frame rate, or if another
// process is writing to the node, which the index exists to survive.
func (t *loopbackBindingTable) Record(b loopbackBinding) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Evict the entry currently occupying this slot, if any. Guarded on count so
	// that an untouched slot's zero value cannot delete a live sequence-0 entry.
	if t.count == len(t.ring) {
		delete(t.index, t.ring[t.next].LoopbackSequence)
	}
	t.ring[t.next] = b
	t.index[b.LoopbackSequence] = t.next
	t.next = (t.next + 1) % len(t.ring)
	if t.count < len(t.ring) {
		t.count++
	}
	if !t.haveNewest || sequenceAfter(b.LoopbackSequence, t.newest) {
		t.newest, t.haveNewest = b.LoopbackSequence, true
	}
}

// Lookup resolves a loopback sequence to the sample it carried.
//
// Returns found=false for a sequence that was never recorded or has aged out of
// the retention window. It deliberately does NOT interpolate or return a nearest
// match: a consumer joining model output to episode input must be told "I cannot
// name this frame" rather than handed a plausible neighbour.
func (t *loopbackBindingTable) Lookup(seq uint32) (loopbackBinding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pos, ok := t.index[seq]
	if !ok {
		return loopbackBinding{}, false
	}
	return t.ring[pos], true
}

// Newest returns the most recently recorded binding, if any. It exists so the
// control plane can attach the current sequence to an outgoing sample without
// the pump and the stream having to share more state than the table.
func (t *loopbackBindingTable) Newest() (loopbackBinding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.haveNewest {
		return loopbackBinding{}, false
	}
	pos, ok := t.index[t.newest]
	if !ok {
		return loopbackBinding{}, false
	}
	return t.ring[pos], true
}

// Len reports how many bindings are currently retained.
func (t *loopbackBindingTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// sequenceAfter reports whether a is later than b on the 32-bit sequence
// circle, tolerating wrap.
//
// v4l2_buffer.sequence is __u32 and the kernel's write_position is not reset per
// open, so a long-lived node can wrap. Comparing with plain > would order
// 0xFFFFFFFF before 0x00000001 wrongly. The signed-difference form treats any
// distance under 2^31 as "forward", which is correct for a counter that never
// advances by anything close to that between observations.
func sequenceAfter(a, b uint32) bool {
	return int32(a-b) > 0
}

// sequenceGap reports how many frames a reader missed between two CONSECUTIVELY
// DEQUEUED loopback buffers, given their kernel-assigned sequences.
//
// This is case 2 from the file comment, and it is the reader's own drop signal:
// v4l2loopback fast-forwards an overrun reader instead of blocking it, so the
// jump in sequence is exactly the loss. A normal delivery differs by one and
// yields zero.
//
// Returns 0 when curr is not after prev, which covers both a repeated buffer
// (v4l2loopback re-serves the newest frame to a reader that has caught up, so
// the same sequence can be dequeued twice) and any out-of-order observation. A
// repeat is not a loss and must not be reported as one.
func sequenceGap(prev, curr uint32) uint64 {
	if !sequenceAfter(curr, prev) {
		return 0
	}
	return uint64(curr-prev) - 1
}

// frameBindableToLoopback reports whether a hub frame can be written to a
// v4l2loopback node in a way that preserves the one-frame-in-one-frame-out
// correspondence the binding depends on, and explains the refusal when it
// cannot.
//
// This predicate is where the honest scope of the feature is enforced, so the
// reasoning is worth stating in full.
//
// The binding is only meaningful if each frame the pump takes off the hub
// becomes exactly one frame on the node. Two things break that:
//
//   - A frame that is not a whole access unit. auAligned is set only by the
//     native V4L2 producer, which delivers one V4L2 buffer per encoded frame.
//     The GStreamer and IP camera producers read a byte stream from a pipe, so
//     their frames are arbitrary chunk-sized slices that can begin or end
//     mid-access-unit. Writing those to a node frame-for-frame produces buffers
//     that are not frames, and a sequence that counts chunks rather than
//     pictures. There is no binding to be had, so we refuse rather than publish
//     a sequence that means nothing.
//
//   - A decoder placed between the hub and the node. Feeding the raw-pixel
//     consumers (notably OpenCV's default V4L capture, which will not accept a
//     compressed fourcc) would mean decoding H.264 in the pump and writing raw
//     frames. A decoder does not preserve the correspondence: B-frame
//     reordering means the Nth frame in is not the Nth frame out, and a decoder
//     may drop or emit frames on its own. The Nth-write-is-the-Nth-sample
//     inference would then be silently wrong. So this path does not decode, and
//     consequently does not serve raw-pixel consumers at all. That is a real
//     limitation of the feature, not an omission to be patched later by adding a
//     decoder: adding one would destroy the guarantee the feature exists to
//     make.
//
// What remains is the sound subset: whole compressed access units written to a
// node advertising the matching compressed fourcc, consumed by tools that accept
// one (ffmpeg and GStreamer do).
func frameBindableToLoopback(frame *videoFrame) (ok bool, reason string) {
	switch {
	case frame == nil:
		return false, "no frame"
	case len(frame.data) == 0:
		return false, "empty frame"
	case !frame.auAligned:
		// Refusing here is the difference between a sequence that names pictures
		// and one that names arbitrary byte chunks.
		return false, "source delivers an unaligned byte stream, not whole access units; " +
			"a loopback sequence over it would not identify frames"
	case frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264:
		// Only H.264 has a settled V4L2 pixel format we can advertise on the node
		// (V4L2_PIX_FMT_H264). VP8 arrives from the hub inside a WebM container
		// rather than as bare frames, so it is not a per-frame payload at all.
		return false, "only h264 access units can be written to a loopback node frame-for-frame"
	}
	return true, ""
}
