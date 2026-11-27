package patrol

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func progressRig(t *testing.T, returning bool) (*Engine, *fake, string, string) {
	t.Helper()
	if returning {
		e, f := returnRig(t)
		if err := e.Control("recall", 1000); err != nil {
			t.Fatal(err)
		}
		if err := e.Tick(1000, rn(f, 1)); err != nil {
			t.Fatal(err)
		}
		return e, &f.fake, "returning", "return_held"
	}
	e, f := tourRig(t)
	if err := e.Tick(1000, Navigation{Ready: true}); err != nil {
		t.Fatal(err)
	}
	return e, &f.fake, "navigating", "tour_held"
}

func progressTick(t *testing.T, e *Engine, f *fake, now int64, distance float64) {
	t.Helper()
	n := nav(f)
	n.Stamp = float64(now) / 1000
	n.Goal.Distance = distance
	if err := e.Tick(now, n); err != nil {
		t.Fatal(err)
	}
}

func TestProgressInitialZeroDoesNotPoisonAdvancingDistance(t *testing.T) {
	for _, returning := range []bool{false, true} {
		t.Run(map[bool]string{false: "tour", true: "return"}[returning], func(t *testing.T) {
			e, f, active, held := progressRig(t, returning)
			canceled := f.canceled
			// Nav2 emits an initial zero before it has a computed path length.
			progressTick(t, e, f, 1100, 0)
			for i := int64(0); i < 5; i++ {
				progressTick(t, e, f, 2000+i*5000, 13-float64(i))
				if e.State.State != active || f.canceled != canceled {
					t.Fatalf("advancing route was canceled: %+v", e.State)
				}
			}
			// The same watchdog still holds exactly 15 seconds after progress.
			progressTick(t, e, f, 36999, 9)
			if e.State.State != active {
				t.Fatal("watchdog fired before 15 seconds")
			}
			progressTick(t, e, f, 37000, 9)
			if e.State.State != held || f.canceled != canceled+1 {
				t.Fatal("stalled route did not cancel", e.State)
			}
		})
	}
}

func TestProgressInvalidDistanceCannotRenewWatchdog(t *testing.T) {
	var decoded Navigation
	if err := json.Unmarshal([]byte(`{"goal":{"distance_remaining":null}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, returning := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			value float64
		}{{"zero", 0}, {"json-null", decoded.Goal.Distance}, {"negative", -1}, {"nan", math.NaN()}, {"infinity", math.Inf(1)}} {
			t.Run(map[bool]string{false: "tour/", true: "return/"}[returning]+tc.name, func(t *testing.T) {
				e, f, active, held := progressRig(t, returning)
				canceled := f.canceled
				for _, now := range []int64{1100, 5000, 10000, 15999} {
					progressTick(t, e, f, now, tc.value)
					if e.State.State != active {
						t.Fatal("watchdog fired early", e.State)
					}
				}
				progressTick(t, e, f, 16000, tc.value)
				if e.State.State != held || f.canceled != canceled+1 || !strings.Contains(e.State.Reason, "no navigation progress for 15 seconds") {
					t.Fatal("invalid feedback renewed progress", e.State)
				}
			})
		}
	}
}

func TestProgressOlderLargerDistanceDoesNotRenewWatchdog(t *testing.T) {
	for _, returning := range []bool{false, true} {
		t.Run(map[bool]string{false: "tour", true: "return"}[returning], func(t *testing.T) {
			e, f, _, held := progressRig(t, returning)
			progressTick(t, e, f, 2000, 8)
			progressTick(t, e, f, 10000, 9)
			progressTick(t, e, f, 16999, 8)
			progressTick(t, e, f, 17000, 9)
			if e.State.State != held {
				t.Fatal("larger or repeated feedback counted as progress", e.State)
			}
		})
	}
}
