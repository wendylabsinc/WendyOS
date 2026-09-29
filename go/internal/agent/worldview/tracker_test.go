package worldview

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func match(x, y, conf float64) Match {
	return Match{Box: [4]float64{x, y, 100, 100}, Fused: Fused{Confidence: conf, Matched: true}}
}

func kinds(events []Lifecycle) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.Kind+" "+e.TrackID)
	}
	return out
}

func TestTrackerSteadyTrackEmitsOnlyAppeared(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	var all []Lifecycle
	for i := 0; i < 100; i++ {
		// A few pixels of jitter and a sub-delta confidence wobble.
		jitter := float64(i%3) * 2
		all = append(all, tr.Observe("cam0", "bottle", at(i*100), []Match{match(200+jitter, 200-jitter, 0.8+0.01*float64(i%2))})...)
	}
	if got, want := kinds(all), []string{"appeared cam0/bottle/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	first := all[0]
	if first.Object != "bottle" || first.SourceID != "cam0" || first.Match == nil || !first.At.Equal(t0) {
		t.Fatalf("appeared event fields: %+v", first)
	}
}

func TestTrackerMoved(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	var got []string
	for i := 0; i <= 6; i++ {
		// 10 px per frame on a 100 px box; MoveFraction 0.25 means more than 25 px.
		for _, e := range tr.Observe("cam0", "bottle", at(i*100), []Match{match(float64(10*i), 0, 0.8)}) {
			got = append(got, e.Kind+"@"+e.At.Sub(t0).String())
		}
	}
	want := []string{"appeared@0s", "moved@300ms", "moved@600ms"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTrackerMovedUsesAnchorBoxLargerSide(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	tall := func(x float64) []Match {
		return []Match{{Box: [4]float64{x, 0, 100, 200}, Fused: Fused{Confidence: 0.8}}}
	}
	tr.Observe("cam0", "bottle", at(0), tall(0))
	// 40 px is more than 25 percent of the 100 px width but not of the 200 px height.
	if events := tr.Observe("cam0", "bottle", at(100), tall(40)); len(events) != 0 {
		t.Fatalf("moved against the smaller side: %v", kinds(events))
	}
	// 55 px from the box at the last emitted event (x = 0) exceeds 50 px, although
	// it is only 15 px from the previous frame.
	if got, want := kinds(tr.Observe("cam0", "bottle", at(200), tall(55))), []string{"moved cam0/bottle/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if tr.Suppressed() != 0 {
		t.Fatalf("a new track was started instead of associating: suppressed %d", tr.Suppressed())
	}
}

func TestTrackerPeak(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	confidences := []float64{0.8, 0.82, 0.85, 0.87, 0.6, 0.89, 0.9, 0.95}
	var got []string
	for i, c := range confidences {
		for _, e := range tr.Observe("cam0", "bottle", at(i*100), []Match{match(0, 0, c)}) {
			got = append(got, e.Kind)
			if e.Kind == KindPeak && e.Match.Fused.Confidence != c {
				t.Fatalf("peak carries confidence %v, want %v", e.Match.Fused.Confidence, c)
			}
		}
	}
	// 0.85 is 0.05 above 0.80, 0.90 is 0.05 above 0.85, 0.95 is 0.05 above 0.90.
	if want := []string{"appeared", "peak", "peak", "peak"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTrackerLostFiresOnce(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)})
	var got []string
	record := func(events []Lifecycle) {
		for _, e := range events {
			got = append(got, e.Kind+"@"+e.At.Sub(t0).String())
			if e.Kind == KindLost && e.Match != nil {
				t.Fatalf("lost carries a match")
			}
		}
	}
	record(tr.Observe("cam0", "bottle", at(1000), nil))
	record(tr.Expire(at(1999)))
	record(tr.Observe("cam0", "bottle", at(2000), nil))
	record(tr.Observe("cam0", "bottle", at(3000), nil))
	record(tr.Expire(at(10000)))
	if want := []string{"lost@2s"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTrackerExpireLost(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	tr.Observe("cam1", "cup", at(0), []Match{match(0, 0, 0.8)})
	tr.Observe("cam0", "bottle", at(500), []Match{match(0, 0, 0.8)})
	tr.Observe("cam0", "apple", at(500), []Match{match(0, 0, 0.8)})
	if events := tr.Expire(at(1999)); len(events) != 0 {
		t.Fatalf("expired early: %v", kinds(events))
	}
	if got, want := kinds(tr.Expire(at(2500))), []string{"lost cam0/apple/1", "lost cam0/bottle/1", "lost cam1/cup/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expire = %v, want %v", got, want)
	}
	if events := tr.Expire(at(9000)); len(events) != 0 {
		t.Fatalf("lost fired twice: %v", kinds(events))
	}
}

func TestTrackerObserveLosesOnlySameSourceAndObject(t *testing.T) {
	tr := NewTracker(time.Second, 0)
	tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)})
	tr.Observe("cam0", "cup", at(0), []Match{match(0, 0, 0.8)})
	tr.Observe("cam1", "bottle", at(0), []Match{match(0, 0, 0.8)})
	if got, want := kinds(tr.Observe("cam0", "bottle", at(1500), nil)), []string{"lost cam0/bottle/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("observe = %v, want %v", got, want)
	}
	if got, want := kinds(tr.Expire(at(1500))), []string{"lost cam0/cup/1", "lost cam1/bottle/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expire = %v, want %v", got, want)
	}
}

func TestTrackerReappearAfterLostIsNewTrack(t *testing.T) {
	tr := NewTracker(time.Second, 0)
	var got []string
	got = append(got, kinds(tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)}))...)
	// Same box after the clear window: lost first, then a new track.
	got = append(got, kinds(tr.Observe("cam0", "bottle", at(1000), []Match{match(0, 0, 0.8)}))...)
	want := []string{"appeared cam0/bottle/1", "lost cam0/bottle/1", "appeared cam0/bottle/2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// fusedMatches runs Fuse the way the service will and passes only matched
// proposals to the tracker.
func fusedMatches(score float64) []Match {
	f := Fuse([]Evidence{avail("shape", score)}, FusionSpec{Threshold: 0.7})
	if !f.Matched {
		return nil
	}
	return []Match{{Box: [4]float64{0, 0, 100, 100}, Fused: f}}
}

func TestTrackerFlappingWithCooldownAppearsOnce(t *testing.T) {
	cases := []struct {
		name       string
		clearAfter time.Duration
		lost       int
		suppressed int
	}{
		// Clear window longer than every gap: one track throughout.
		{"gaps shorter than clear window", 2 * time.Second, 0, 0},
		// Clear window shorter than the gaps: tracks end and restart inside the cooldown.
		{"gaps longer than clear window", 300 * time.Millisecond, 1, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTracker(c.clearAfter, 30*time.Second)
			counts := map[string]int{}
			for i := 0; i < 80; i++ {
				// Three frames at 0.72, then five at 0.68, around a 0.7 threshold.
				score := 0.68
				if i%8 < 3 {
					score = 0.72
				}
				for _, e := range tr.Observe("cam0", "bottle", at(i*100), fusedMatches(score)) {
					counts[e.Kind]++
				}
			}
			if counts[KindAppeared] != 1 {
				t.Fatalf("appeared %d times, want 1 (%v)", counts[KindAppeared], counts)
			}
			if counts[KindLost] != c.lost || tr.Suppressed() != c.suppressed {
				t.Fatalf("lost %d suppressed %d, want %d and %d", counts[KindLost], tr.Suppressed(), c.lost, c.suppressed)
			}
		})
	}
}

func TestTrackerSilentTrackAnnouncesAfterCooldown(t *testing.T) {
	tr := NewTracker(500*time.Millisecond, 5*time.Second)
	tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)})
	tr.Observe("cam0", "bottle", at(1000), nil) // lost
	var got []string
	for ms := 1100; ms <= 6000; ms += 100 {
		for _, e := range tr.Observe("cam0", "bottle", at(ms), []Match{match(0, 0, 0.8)}) {
			got = append(got, e.Kind+" "+e.TrackID+"@"+e.At.Sub(t0).String())
		}
	}
	if want := []string{"appeared cam0/bottle/2@5s"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if tr.Suppressed() != 1 {
		t.Fatalf("suppressed = %d, want 1", tr.Suppressed())
	}
}

func TestTrackerSilentTrackIsNotLost(t *testing.T) {
	tr := NewTracker(500*time.Millisecond, 5*time.Second)
	tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)})
	tr.Observe("cam0", "bottle", at(1000), []Match{match(0, 0, 0.8)}) // lost /1, silent /2
	if events := tr.Expire(at(3000)); len(events) != 0 {
		t.Fatalf("silent track emitted: %v", kinds(events))
	}
}

func TestTrackerAssociatesMultipleObjects(t *testing.T) {
	tr := NewTracker(2*time.Second, 0)
	left, right := match(0, 0, 0.8), match(500, 0, 0.8)
	if got, want := kinds(tr.Observe("cam0", "bottle", at(0), []Match{left, right})), []string{"appeared cam0/bottle/1", "appeared cam0/bottle/2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first frame = %v, want %v", got, want)
	}
	// Reversed order in the next frame must associate by overlap, not position.
	if events := tr.Observe("cam0", "bottle", at(100), []Match{right, left}); len(events) != 0 {
		t.Fatalf("second frame emitted %v", kinds(events))
	}
	// Only the right one moves.
	got := kinds(tr.Observe("cam0", "bottle", at(200), []Match{match(0, 0, 0.8), match(540, 0, 0.8)}))
	if want := []string{"moved cam0/bottle/2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("third frame = %v, want %v", got, want)
	}
}

func TestTrackerCooldownSilencesSecondInstanceInSameFrame(t *testing.T) {
	tr := NewTracker(2*time.Second, 10*time.Second)
	got := kinds(tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8), match(500, 0, 0.8)}))
	if want := []string{"appeared cam0/bottle/1"}; !reflect.DeepEqual(got, want) || tr.Suppressed() != 1 {
		t.Fatalf("events = %v suppressed %d, want %v and 1", got, tr.Suppressed(), want)
	}
}

func TestTrackerSerialsArePerSourceAndObject(t *testing.T) {
	tr := NewTracker(2*time.Second, 0)
	var got []string
	got = append(got, kinds(tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)}))...)
	got = append(got, kinds(tr.Observe("cam0", "cup", at(0), []Match{match(0, 0, 0.8)}))...)
	got = append(got, kinds(tr.Observe("cam1", "bottle", at(0), []Match{match(0, 0, 0.8)}))...)
	want := []string{"appeared cam0/bottle/1", "appeared cam0/cup/1", "appeared cam1/bottle/1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTrackerDisjointBoxesDoNotAssociate(t *testing.T) {
	tr := NewTracker(2*time.Second, 0)
	tr.IoUMin = 0
	tr.Observe("cam0", "bottle", at(0), []Match{match(0, 0, 0.8)})
	got := kinds(tr.Observe("cam0", "bottle", at(100), []Match{match(1000, 0, 0.8)}))
	if want := []string{"appeared cam0/bottle/2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
