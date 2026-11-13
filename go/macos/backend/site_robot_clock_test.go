package backend

import (
	"testing"
	"time"
)

func TestRobotClockSampleNeverMakesPacketsNewer(t *testing.T) {
	sent := time.Unix(1792969000, 0)
	for _, skew := range []time.Duration{-120 * time.Millisecond, 75 * time.Millisecond, time.Second} {
		for _, outbound := range []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond} {
			rtt := 20 * time.Millisecond
			server := sent.Add(outbound + skew)
			offset, ok := robotClockSample(sent, sent.Add(rtt), server.UnixNano())
			if !ok {
				t.Fatal("valid clock exchange rejected")
			}
			// A subsequent packet is stamped by the gateway 100 ms after sending.
			actual := sent.Add(100 * time.Millisecond)
			translated := actual.Add(skew).Add(-offset)
			if translated.After(actual) || actual.Sub(translated) > rtt {
				t.Fatalf("offset hid age or exceeded uncertainty: %v %v", skew, outbound)
			}
		}
	}
	for _, rtt := range []time.Duration{-time.Millisecond, 51 * time.Millisecond, time.Second} {
		if _, ok := robotClockSample(sent, sent.Add(rtt), sent.UnixNano()); ok {
			t.Fatal("accepted uncertain clock exchange")
		}
	}
	if _, ok := robotClockSample(sent, sent, 0); ok {
		t.Fatal("accepted missing clock")
	}
}
