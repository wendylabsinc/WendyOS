package ros2sensor

import "time"

const (
	// clockWindow is how long a fast arrival keeps counting as the fastest.
	// Two windows are kept, so the estimate rests on the last 10 to 20
	// seconds: long enough to see a fast frame on a slow link, short enough
	// that the clocks have not drifted far apart since.
	clockWindow = 10 * time.Second
	// clockJump is how much sooner than expected a frame has to arrive before
	// the source's clock is taken to have been set ahead. Nothing arrives
	// before it was sent.
	clockJump = 2 * time.Second
	// clockDriftPPM bounds how fast a source's clock and this device's move
	// apart, in parts per million. Crystal oscillators stay well inside it.
	clockDriftPPM = 100
)

// sourceClock places times a source read from its own clock on this device's
// clock.
//
// The two clocks share no epoch: a Wendy Lite counts from its boot. A frame
// its source stamped s arrives here at a, so the clocks differ by a - s less
// the time the frame took to get here. That time is unknown and never
// negative, and it is smallest for the frame that arrived fastest. The
// smallest a - s seen lately is therefore the best estimate of the offset. It
// is late by the fastest transit time, which a stream that only flows one way
// cannot measure.
//
// The offset in use follows that estimate in steps of at most half a sample
// period. Samples the source took in order are then placed in order, and
// samples it took a period apart are placed a period apart unless the
// estimate itself moved.
//
// A source whose clock goes backwards (it restarted) or jumps ahead starts a
// new segment: what the old offset said no longer holds.
type sourceClock struct {
	segment     int
	offset      time.Duration // in use: device time = source time + offset
	lastSource  time.Duration
	bucketStart time.Duration
	cur, prev   fastest
	havePrev    bool
}

// fastest is the smallest a - s of one window, and when it arrived.
type fastest struct{ delay, arrival time.Duration }

func (c *sourceClock) best() fastest {
	if c.havePrev && c.prev.delay < c.cur.delay {
		return c.prev
	}
	return c.cur
}

// place returns when, on this device's clock, a sample was taken that its
// source stamped source and that arrived at arrival. period is the time
// between the source's samples, or zero when it is not known.
//
// uncertainty is how far that may be from the best estimate: the distance the
// offset in use still has to move, plus what the clocks may have drifted
// since the fastest arrival. It does not cover the fastest transit time.
func (c *sourceClock) place(source, arrival, period time.Duration) (taken, uncertainty time.Duration) {
	delay := arrival - source
	if c.segment == 0 || source < c.lastSource || delay < c.offset-clockJump {
		c.segment++
		c.offset = delay
		c.cur, c.havePrev, c.bucketStart = fastest{delay, arrival}, false, arrival
	} else {
		if arrival-c.bucketStart >= clockWindow {
			c.prev, c.havePrev = c.cur, true
			c.cur, c.bucketStart = fastest{delay, arrival}, arrival
		} else if delay < c.cur.delay {
			c.cur = fastest{delay, arrival}
		}
		step := period / 2
		if period <= 0 {
			step = (source - c.lastSource) / 2
		}
		c.offset += max(-step, min(step, c.best().delay-c.offset))
	}
	c.lastSource = source

	// A sample was not taken after it arrived, whatever the offset says while
	// it is still catching up.
	taken = min(source+c.offset, arrival)
	best := c.best()
	behind := taken - (source + best.delay)
	if behind < 0 {
		behind = -behind
	}
	return taken, behind + (arrival-best.arrival)*clockDriftPPM/1_000_000
}
