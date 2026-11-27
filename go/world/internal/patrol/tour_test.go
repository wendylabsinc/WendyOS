package patrol

import (
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"math"
	"strings"
	"testing"
)

type tourFake struct {
	fake
	stamp   float64
	delta   float64
	samples int
}

func (f *tourFake) Capture() (Capture, error) {
	f.captures++
	d := strings.Repeat("a", 63) + string(rune('0'+f.captures))
	return Capture{TimeS: f.stamp + f.delta, SampleCount: f.samples, ManifestSHA256: d, Evidence: []string{d, strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)}}, nil
}
func tourRig(t *testing.T) (*Engine, *tourFake) {
	t.Helper()
	f := &tourFake{fake: fake{unreachable: map[float64]bool{}}, samples: 5}
	e, err := New("tour", 100000, []Room{room("far", 4), room("near", 2)}, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SetReturn(Point{X: 1}, 0); err != nil {
		t.Fatal(err)
	}
	p := patroltour.Policy{Method: patroltour.Method, Stops: []patroltour.Stop{{RoomID: "far", Position: []float64{4, .3, 0}}, {RoomID: "near", Position: []float64{2, .3, 0}}}}
	if err = e.SetTour(p); err != nil {
		t.Fatal(err)
	}
	p.Stops[0].Position[0] = 9
	return e, f
}
func tourObserve(t *testing.T, e *Engine, f *tourFake, base int64, yaw float64) {
	t.Helper()
	n := nav(&f.fake)
	n.Goal.State = "succeeded"
	n.Pose.Yaw = yaw
	for i := 0; i <= 4; i++ {
		n.Stamp = float64(base)/1000 + float64(i)*.5
		f.stamp = n.Stamp
		if err := e.Tick(base+int64(i)*500, n); err != nil {
			t.Fatal(err)
		}
	}
}
func TestFiniteTourOrderedOriginalCapturesThenReturn(t *testing.T) {
	e, f := tourRig(t)
	_ = e.Tick(1000, Navigation{Ready: true})
	if f.p.X != 4 {
		t.Fatal("ignored signed order or aliased policy", f.p)
	}
	tourObserve(t, e, f, 2000, 0)
	if len(e.State.Tour.Visits) != 1 || f.captures != 1 {
		t.Fatalf("capture missing: %+v", e.State)
	}
	_ = e.Tick(4500, Navigation{Ready: true})
	if f.p.X != 2 {
		t.Fatal("did not select next signed stop")
	}
	tourObserve(t, e, f, 5000, 0)
	_ = e.Tick(7500, Navigation{Ready: true})
	if !e.State.Return.Requested || e.State.Return.Trigger != "inspection_complete" || !e.TourComplete() {
		t.Fatal(e.State)
	}
	proof := e.TourEvidence()
	proof.Visits[0].Evidence[0] = "mutated"
	if !e.TourComplete() {
		t.Fatal("result aliases caller")
	}
}
func TestTourBlockedAndEarlyRecallRemainIncomplete(t *testing.T) {
	e, f := tourRig(t)
	f.unreachable[4] = true
	_ = e.Tick(1000, Navigation{Ready: true})
	if e.State.State != "tour_held" || f.goal != "" || len(e.State.Tour.Visits) != 0 {
		t.Fatal(e.State)
	}
	f.unreachable[4] = false
	_ = e.Tick(2000, Navigation{Ready: true})
	if f.goal != "" {
		t.Fatal("automatic resume")
	}
	if err := e.Control("resume", 2100); err != nil {
		t.Fatal(err)
	}
	_ = e.Tick(2200, Navigation{Ready: true})
	if f.p.X != 4 {
		t.Fatal("blocked stop skipped")
	}
	if err := e.Control("recall", 2300); err != nil {
		t.Fatal(err)
	}
	if e.TourComplete() || e.State.Tour.Outcome != "incomplete" {
		t.Fatal("early recall invented observations")
	}
	_ = e.Control("stop", 2400)
	before := f.goal
	_ = e.Tick(2500, Navigation{Ready: true})
	if f.goal != before || e.State.State != "stopped" {
		t.Fatal("Stop lost")
	}
}
func TestTourRejectsWrongYawOldFutureAndMissingCaptures(t *testing.T) {
	for _, kind := range []string{"yaw", "old", "future", "missing", "gap"} {
		t.Run(kind, func(t *testing.T) {
			e, f := tourRig(t)
			_ = e.Tick(1000, Navigation{Ready: true})
			yaw := 0.
			switch kind {
			case "yaw":
				yaw = 1
			case "old":
				f.delta = -3
			case "future":
				f.delta = 2
			case "missing":
				f.samples = 4
			}
			if kind == "gap" {
				n := nav(&f.fake)
				n.Goal.State = "succeeded"
				n.Stamp = 2
				_ = e.Tick(2000, n)
				n.Stamp = 12
				f.stamp = 12
				_ = e.Tick(12000, n)
			} else {
				tourObserve(t, e, f, 2000, yaw)
			}
			if len(e.State.Tour.Visits) != 0 || e.State.Return.Requested {
				t.Fatal("unsupported observation counted", e.State)
			}
		})
	}
}

func TestTourRejectsNonFinitePose(t *testing.T) {
	for _, axis := range []string{"x", "y", "yaw"} {
		for _, value := range []float64{math.NaN(), math.Inf(1)} {
			t.Run(axis, func(t *testing.T) {
				e, f := tourRig(t)
				_ = e.Tick(1000, Navigation{Ready: true})
				n := nav(&f.fake)
				n.Goal.State = "succeeded"
				switch axis {
				case "x":
					n.Pose.X = value
				case "y":
					n.Pose.Y = value
				case "yaw":
					n.Pose.Yaw = value
				}
				for i := 0; i <= 4; i++ {
					n.Stamp = 2 + float64(i)*.5
					f.stamp = n.Stamp
					_ = e.Tick(2000+int64(i)*500, n)
				}
				if len(e.State.Tour.Visits) != 0 || f.captures != 0 {
					t.Fatal("non-finite pose counted as observed")
				}
			})
		}
	}
}
