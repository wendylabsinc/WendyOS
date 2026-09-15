package services

import (
	"encoding/binary"
	"testing"
	"time"
)

// TestV4L2BufferTimestampSetterRoundTrip pins the in-band frame identity the
// two-plane data path writes onto every buffer it queues.
//
// This is a byte-layout test on purpose, and it asserts the RAW BYTES rather
// than only round-tripping through the accessors. v4l2Buf is a fixed-size array
// whose fields are reached by hand-computed offsets precisely because a Go
// struct cannot be trusted to match a C struct containing a union and an
// alignment-forcing struct timeval. A setter and a getter that agree with each
// other while both pointing at the wrong offset would round-trip perfectly and
// still put the timestamp somewhere the kernel never reads. So the offsets are
// checked directly, against the same 12 seconds plus 3456 microseconds that the
// decode test TestV4L2BufferTimestampMetadata pins from the other direction.
func TestV4L2BufferTimestampSetterRoundTrip(t *testing.T) {
	// A receipt with a sub-microsecond remainder, so the truncation struct
	// timeval forces is actually exercised rather than being a no-op.
	const nanos int64 = 12*int64(time.Second) + 3456*int64(time.Microsecond) + 789

	var buffer v4l2Buf
	buffer.setTimestampNanos(nanos)

	// tv_sec at offset 24 and tv_usec at offset 32, the layout timestampNanos
	// reads. Written as unsigned here only because that is how the decode test
	// stages the same two fields; the values are positive so the bits match.
	if sec := binary.LittleEndian.Uint64(buffer[24:32]); sec != 12 {
		t.Errorf("tv_sec at offset 24 = %d, want 12", sec)
	}
	if usec := binary.LittleEndian.Uint64(buffer[32:40]); usec != 3456 {
		t.Errorf("tv_usec at offset 32 = %d, want 3456", usec)
	}

	// Nothing may have landed outside those two fields. bytesused, flags and
	// sequence all live in the same struct and a stray write into one of them
	// would corrupt the queued buffer in a way no round-trip would reveal.
	if buffer.bytesUsed() != 0 || buffer.flags() != 0 || buffer.sequence() != 0 || buffer.index() != 0 {
		t.Errorf("setTimestampNanos disturbed a neighbouring field: index=%d bytesused=%d flags=%#x sequence=%d",
			buffer.index(), buffer.bytesUsed(), buffer.flags(), buffer.sequence())
	}

	// The existing reader must see the value back, truncated to whole
	// microseconds because struct timeval has no finer resolution.
	wantTruncated := 12*int64(time.Second) + 3456*int64(time.Microsecond)
	if got := buffer.timestampNanos(); got != wantTruncated {
		t.Errorf("timestampNanos() = %d, want %d", got, wantTruncated)
	}

	// The identity a consumer relies on: it derives the expected buffer
	// timestamp from FrameIdentity.boottime_nanos by dividing by 1000, with no
	// second field on the wire. That works only because 1e9 divides evenly by
	// 1000, which makes the truncation exact rather than approximate.
	sec := int64(binary.LittleEndian.Uint64(buffer[24:32]))
	usec := int64(binary.LittleEndian.Uint64(buffer[32:40]))
	if got, want := sec*1_000_000+usec, nanos/1000; got != want {
		t.Errorf("sec*1e6+usec = %d, want boottime_nanos/1000 = %d", got, want)
	}

	// And the flag that tells a reader the timestamp is the writer's own rather
	// than a clock the module read for us.
	buffer.setFlags(v4l2BufFlagTimestampCopy)
	if got := buffer.flags(); got != 0x4000 {
		t.Errorf("flags() = %#x after setFlags(v4l2BufFlagTimestampCopy), want 0x4000", got)
	}
	// Setting the flags must not have disturbed the timestamp either: they are
	// adjacent fields in the same 88-byte array.
	if got := buffer.timestampNanos(); got != wantTruncated {
		t.Errorf("timestampNanos() = %d after setFlags, want %d", got, wantTruncated)
	}
}
