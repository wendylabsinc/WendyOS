package ros2sensor

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// boardOffset is what a board's clock has to be moved by to read this
// device's clock: the tests' boards booted 90 seconds after the channel's
// clock started.
const boardOffset = 90 * time.Second

func TestSampleTimesKeepTheSourcesSpacingWhileArrivalJitters(t *testing.T) {
	var c sourceClock
	transit := []time.Duration{ms(50), ms(5), ms(80), ms(120), ms(5), ms(60)}
	var taken []time.Duration
	for i, delay := range transit {
		source := time.Duration(i+1) * time.Second
		got, _ := c.place(source, 0, source+boardOffset+delay, time.Second)
		taken = append(taken, got)
	}
	// From the frame that arrived fastest on, samples the board took a second
	// apart are placed a second apart, however late each one arrived.
	for i := 2; i < len(taken); i++ {
		if gap := taken[i] - taken[i-1]; gap != time.Second {
			t.Fatalf("samples %d and %d are placed %s apart; the board took them 1s apart", i, i+1, gap)
		}
	}
	// And each is placed as close to when it was taken as the fastest arrival
	// allows: 5 ms late, not the 60 ms its own frame took.
	if late := taken[5] - (6*time.Second + boardOffset); late != ms(5) {
		t.Fatalf("last sample is placed %s after it was taken, want the fastest transit time, 5ms", late)
	}
	if c.segment != 1 {
		t.Fatalf("jitter alone started mapping segment %d", c.segment)
	}
}

func TestSampleIsNeverPlacedAfterItArrivedOrBeforeTheOneBeforeIt(t *testing.T) {
	var c sourceClock
	const period = time.Millisecond
	seed := uint32(1)
	var previous, lastArrival time.Duration
	for i := 1; i <= 5000; i++ {
		seed = seed*1664525 + 1013904223
		delay := time.Duration(seed>>16%20000) * time.Microsecond // 0 to 20 ms
		source := time.Duration(i) * period
		// Frames arrive in the order they were sent.
		arrival := max(source+boardOffset+delay, lastArrival)
		lastArrival = arrival
		taken, _ := c.place(source, 0, arrival, period)
		if taken > arrival {
			t.Fatalf("sample %d is placed %s after it arrived", i, taken-arrival)
		}
		if taken < previous {
			t.Fatalf("sample %d is placed %s before sample %d", i, previous-taken, i-1)
		}
		previous = taken
	}
}

func TestFrameHeldUpOnTheLinkIsPlacedWhenItWasTaken(t *testing.T) {
	var c sourceClock
	for i := 1; i <= 5; i++ {
		source := time.Duration(i) * time.Second
		c.place(source, 0, source+boardOffset+ms(5), time.Second)
	}
	// The link stalls for three seconds, then delivers the sixth frame.
	source := 6 * time.Second
	taken, _ := c.place(source, 0, source+boardOffset+3*time.Second, time.Second)
	if late := taken - (source + boardOffset); late != ms(5) {
		t.Fatalf("a frame that arrived 3s late is placed %s after it was taken, want 5ms", late)
	}
	if c.segment != 1 {
		t.Fatalf("a late frame started mapping segment %d", c.segment)
	}
}

func TestSourceClockThatStartsAgainStartsANewSegment(t *testing.T) {
	var c sourceClock
	c.place(100*time.Second, 0, 100*time.Second+boardOffset+ms(5), time.Second)
	c.place(101*time.Second, 0, 101*time.Second+boardOffset+ms(5), time.Second)
	// The board restarted: its clock counts from zero again.
	arrival := 104*time.Second + boardOffset
	taken, _ := c.place(time.Second, 0, arrival, time.Second)
	if c.segment != 2 {
		t.Fatalf("mapping segment %d after the source clock went backwards, want 2", c.segment)
	}
	// Nothing is known yet about the new clock but when this frame arrived.
	if taken != arrival {
		t.Fatalf("first sample of the new segment is placed %s from its arrival", arrival-taken)
	}
	// The old offset is gone: the next sample follows the new clock.
	next, _ := c.place(2*time.Second, 0, arrival+time.Second+ms(40), time.Second)
	if next-taken != time.Second {
		t.Fatalf("second sample of the new segment is %s after the first, want 1s", next-taken)
	}
}

func TestSourceClockThatJumpsAheadStartsANewSegment(t *testing.T) {
	var c sourceClock
	c.place(10*time.Second, 0, 10*time.Second+boardOffset+ms(5), time.Second)
	c.place(11*time.Second, 0, 11*time.Second+boardOffset+ms(5), time.Second)
	// A second later the board's clock reads ten seconds more.
	arrival := 12*time.Second + boardOffset + ms(5)
	taken, _ := c.place(21*time.Second, 0, arrival, time.Second)
	if c.segment != 2 || taken != arrival {
		t.Fatalf("segment %d, placed %s from arrival; want a new segment placed at arrival", c.segment, arrival-taken)
	}
}

func TestSourceClockThatRunsSlowOrFastIsFollowed(t *testing.T) {
	for name, skew := range map[string]time.Duration{"slow": -100 * time.Microsecond, "fast": 100 * time.Microsecond} {
		var c sourceClock
		// 100 ppm off, for twenty minutes, one frame a second, 2 ms in transit.
		for i := 1; i <= 1200; i++ {
			tookAt := time.Duration(i)*time.Second + boardOffset // on the device's clock
			source := time.Duration(i) * (time.Second + skew)
			taken, uncertainty := c.place(source, 0, tookAt+ms(2), time.Second+skew)
			late := taken - tookAt - ms(2)
			if late < 0 {
				late = -late
			}
			// The drift since the fastest recent arrival is all that is
			// unaccounted for, and the uncertainty reported covers it.
			if late > ms(3) || late > uncertainty {
				t.Fatalf("%s clock, sample %d: placed %s off with a reported uncertainty of %s", name, i, late, uncertainty)
			}
		}
		if c.segment != 1 {
			t.Fatalf("%s clock: drift started mapping segment %d", name, c.segment)
		}
	}
}

func TestUncertaintyCoversAnOffsetStillCatchingUp(t *testing.T) {
	var c sourceClock
	const period = 20 * time.Millisecond
	// The first frame took 50 ms, the second 5 ms, the rest 30 ms. The offset
	// starts 45 ms late and moves at most half a period per frame, so that a
	// later sample never overtakes an earlier one.
	transit := []time.Duration{ms(50), ms(5), ms(30), ms(30), ms(30), ms(30), ms(30)}
	var late []time.Duration
	for i, delay := range transit {
		source := time.Duration(i+1) * period
		taken, uncertainty := c.place(source, 0, source+boardOffset+delay, period)
		// Against the best that can be known: the fastest transit, 5 ms.
		behind := taken - (source + boardOffset + ms(5))
		if i > 0 && (behind < 0 || behind > uncertainty) {
			t.Fatalf("sample %d: placed %s late with a reported uncertainty of %s", i+1, behind, uncertainty)
		}
		late = append(late, behind)
	}
	// 40, 30, 20, 10 ms late for samples 2 to 5, less whatever their own
	// arrival rules out; caught up from sample 6 on.
	want := []time.Duration{ms(45), 0, ms(25), ms(15), ms(5), 0, 0}
	for i := range want {
		if late[i] != want[i] {
			t.Fatalf("samples are placed %v late, want %v", late, want)
		}
	}
}

func TestBatchOfReadingsIsPlacedByWhenItsLastOneWasTaken(t *testing.T) {
	var c sourceClock
	// Ten readings a frame, 100 ms apart. A frame is stamped with its first
	// reading's time and cannot leave before its last one is taken, 900 ms
	// later. That wait is not transit time, and must not push the stamp late.
	const period, span = 100 * time.Millisecond, 900 * time.Millisecond
	for i := 1; i <= 5; i++ {
		source := time.Duration(i) * time.Second
		arrival := source + span + boardOffset + ms(5)
		taken, _ := c.place(source, span, arrival, period)
		if late := taken - (source + boardOffset); late != ms(5) {
			t.Fatalf("frame %d: first reading is placed %s after it was taken, want the transit time, 5ms", i, late)
		}
		if last := taken + span; last > arrival {
			t.Fatalf("frame %d: last reading is placed %s after the frame arrived", i, last-arrival)
		}
	}
}

func TestFramesCloserTogetherThanTheSourceSaidAreStillPlacedInOrder(t *testing.T) {
	var c sourceClock
	// The source says it reads once a second and sends every 100 ms. The
	// first frame was held up 400 ms; the offset may not move by more than
	// the frames are apart, or the second reading would land before the first.
	first, _ := c.place(time.Second, 0, time.Second+boardOffset+ms(400), time.Second)
	second, _ := c.place(time.Second+ms(100), 0, time.Second+ms(100)+boardOffset+ms(405), time.Second)
	if second <= first {
		t.Fatalf("the second reading is placed %s before the first", first-second)
	}
}

func TestSegmentNumbersCarryOnWhereAnEarlierConnectionStopped(t *testing.T) {
	// A source that reconnects starts a new connection and often a new clock:
	// its segments are numbered on from the last one, never from one again.
	c := sourceClock{segment: 3}
	c.place(time.Second, 0, time.Second+boardOffset, time.Second)
	if c.segment != 4 {
		t.Fatalf("the first frame of a later connection is in segment %d, want 4", c.segment)
	}
	c.place(2*time.Second, 0, 2*time.Second+boardOffset, time.Second)
	if c.segment != 4 {
		t.Fatalf("the second frame moved to segment %d", c.segment)
	}
}
