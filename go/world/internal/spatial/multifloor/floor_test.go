package multifloor

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

// ramp is a height profile: linear between (time, height) knots, flat outside.
type ramp [][2]float64

func (r ramp) at(t float64) float64 {
	if t <= r[0][0] {
		return r[0][1]
	}
	for i := 1; i < len(r); i++ {
		if t <= r[i][0] {
			u := (t - r[i-1][0]) / (r[i][0] - r[i-1][0])
			return r[i-1][1] + u*(r[i][1]-r[i-1][1])
		}
	}
	return r[len(r)-1][1]
}

// detect runs a 5 Hz barometer with 0.1 m noise over height(t) and returns
// the moves.
func detect(t *testing.T, seed uint64, seconds float64, height func(float64) float64) []Move {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 10))
	d := NewDetector(FloorConfig{})
	var out []Move
	for k := 0; float64(k)*0.2 < seconds; k++ {
		s := float64(k) * 0.2
		if mv, ok := d.Add(at(s), 17+height(s)+0.1*rng.NormFloat64(), 0.1); ok {
			out = append(out, mv)
		}
	}
	return out
}

func sec(t time.Time) float64 { return t.Sub(t0).Seconds() }

func TestDetectorStairsAndLift(t *testing.T) {
	cases := []struct {
		name               string
		profile            ramp
		delta, start, stop float64
	}{
		{"stairs up", ramp{{20, 0}, {25.5, 3.5}}, 3.5, 20, 25.5},
		{"lift down", ramp{{20, 3.5}, {27, 0}}, -3.5, 20, 27},
		{"express lift", ramp{{20, 0}, {21.75, 3.5}}, 3.5, 20, 21.75},
		{"three storeys", ramp{{20, 0}, {41, 10.5}}, 10.5, 20, 41},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A 5 mm/s drift runs underneath throughout.
			moves := detect(t, 1, 60, func(s float64) float64 { return c.profile.at(s) + 0.005*s })
			if len(moves) != 1 {
				t.Fatalf("moves = %d, want 1: %+v", len(moves), moves)
			}
			mv := moves[0]
			if math.Abs(mv.DeltaM-c.delta) > 0.25 || mv.SigmaM <= 0 || math.Abs(mv.DeltaM-c.delta) > 3*mv.SigmaM {
				t.Errorf("delta = %.2f ± %.2f, want %.2f", mv.DeltaM, mv.SigmaM, c.delta)
			}
			mid := (c.start + c.stop) / 2
			if math.Abs(sec(mv.Mid)-mid) > 0.4 || math.Abs(sec(mv.Start)-c.start) > 0.8 || math.Abs(sec(mv.End)-c.stop) > 0.8 {
				t.Errorf("start, mid, end = %.2f, %.2f, %.2f, want %.2f, %.2f, %.2f", sec(mv.Start), sec(mv.Mid), sec(mv.End), c.start, mid, c.stop)
			}
			// Two dwells that agree, seen through the two-second median.
			if lag := sec(mv.Confirmed) - c.stop; lag < 0 || lag > 8 {
				t.Errorf("confirmed %.1f s after the move ended", lag)
			}
		})
	}
}

func TestDetectorIgnoresDisturbances(t *testing.T) {
	spike := func(from, to, h float64) func(float64) float64 {
		return func(s float64) float64 {
			if s >= from && s < to {
				return h
			}
			return 0
		}
	}
	cases := map[string]func(float64) float64{
		"quiet":          func(float64) float64 { return 0 },
		"door slam up":   spike(30, 30.6, 2.5),
		"door slam down": spike(30, 30.8, -4),
		"weather front":  func(s float64) float64 { return 0.01 * s },                         // 6 m in ten minutes
		"pressure wave":  func(s float64) float64 { return 1.5 * math.Sin(s/40) },             // 3.75 cm/s at most
		"ventilation":    func(s float64) float64 { return ramp{{100, 0}, {101, 1.2}}.at(s) }, // a sustained 1.2 m step
	}
	for name, height := range cases {
		t.Run(name, func(t *testing.T) {
			for seed := uint64(1); seed <= 5; seed++ {
				if moves := detect(t, seed, 600, height); len(moves) != 0 {
					t.Fatalf("seed %d: spurious moves %+v", seed, moves)
				}
			}
		})
	}
}

func TestDetectorLandingPauseIsOneMove(t *testing.T) {
	moves := detect(t, 3, 80, ramp{{20, 0}, {23, 1.75}, {28, 1.75}, {31, 3.5}}.at)
	if len(moves) != 1 || math.Abs(moves[0].DeltaM-3.5) > 0.25 {
		t.Fatalf("moves = %+v, want one of 3.5 m", moves)
	}
}

// A quadruped on the stairs, a wheelchair ramp: the climb is so slow that the
// height never leaves a half-metre band within the dwell, and must still be
// one move of the whole storey, reported when the climb ends.
func TestDetectorSlowClimbIsOneWholeMove(t *testing.T) {
	for _, rate := range []float64{0.1, 0.15, 0.2, 0.3} {
		stop := 20 + 3.5/rate
		for seed := uint64(1); seed <= 4; seed++ {
			moves := detect(t, seed, stop+40, ramp{{20, 0}, {stop, 3.5}}.at)
			if len(moves) != 1 || math.Abs(moves[0].DeltaM-3.5) > 0.25 {
				t.Fatalf("%.2f m/s, seed %d: moves %+v, want one of 3.5 m", rate, seed, moves)
			}
			if mv := moves[0]; mv.Confirmed.Before(at(stop)) || math.Abs(sec(mv.Mid)-(20+stop)/2) > 0.05*(stop-20)+0.5 {
				t.Errorf("%.2f m/s, seed %d: mid %.1f s, confirmed %.1f s; the climb ran from 20 to %.1f s", rate, seed, sec(mv.Mid), sec(mv.Confirmed), stop)
			}
		}
	}
}

// Down the lift, five seconds on the ground, up the stairs: the short stay is
// a floor of its own, known at the latest when the device leaves it again.
func TestDetectorShortStayIsSeen(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		moves := detect(t, seed, 80, ramp{{20, 3.5}, {27, 0}, {32, 0}, {37.5, 3.5}}.at)
		if len(moves) != 2 || math.Abs(moves[0].DeltaM+3.5) > 0.25 || math.Abs(moves[1].DeltaM-3.5) > 0.25 {
			t.Fatalf("seed %d: moves %+v, want 3.5 m down and 3.5 m up", seed, moves)
		}
		if a, b := moves[0], moves[1]; math.Abs(sec(a.Mid)-23.5) > 0.5 || math.Abs(sec(b.Mid)-34.75) > 0.5 || sec(a.Confirmed) > 35 {
			t.Errorf("seed %d: mids %.1f and %.1f s, the first confirmed at %.1f s", seed, sec(a.Mid), sec(b.Mid), sec(a.Confirmed))
		}
	}
}

func TestDetectorAbsorbsStepThenSeesFloor(t *testing.T) {
	// A 1.2 m ventilation step, then a real storey a minute and a half later:
	// the step must not shorten or lengthen the storey.
	moves := detect(t, 4, 200, func(s float64) float64 {
		return ramp{{20, 0}, {21, 1.2}}.at(s) + ramp{{120, 0}, {125.5, 3.5}}.at(s)
	})
	if len(moves) != 1 || math.Abs(moves[0].DeltaM-3.5) > 0.3 {
		t.Fatalf("moves = %+v, want one of 3.5 m", moves)
	}
}

func TestDetectorForgetsBaselineOverGap(t *testing.T) {
	d := NewDetector(FloorConfig{})
	feed := func(from, to, h float64) (n int) {
		for s := from; s < to; s += 0.2 {
			if _, ok := d.Add(at(s), h, 0.1); ok {
				n++
			}
		}
		return n
	}
	if n := feed(0, 20, 5) + feed(80, 100, 11); n != 0 {
		t.Fatalf("%d moves across a 60 s silence; the drift over it is unknown", n)
	}
	if _, ok := d.Add(at(99), 11, 0.1); ok || d.Moving() {
		t.Fatal("a stale sample must be ignored")
	}
	if n := feed(100, 120, 14.5); n != 1 {
		t.Fatalf("%d moves after the baseline was learned again, want 1", n)
	}
}

func TestDetectorMovingAndOffset(t *testing.T) {
	d := NewDetector(FloorConfig{})
	if _, ok := d.Offset(); ok || d.Moving() {
		t.Fatal("no level yet")
	}
	profile := ramp{{20, 0}, {25.5, 3.5}}
	sawMoving := false
	for s := 0.0; s < 40; s += 0.2 {
		_, done := d.Add(at(s), profile.at(s), 0.1)
		if s > 5 && s < 19 && d.Moving() {
			t.Fatalf("moving at %.1f s on a level", s)
		}
		sawMoving = sawMoving || (s > 22 && s < 25 && d.Moving())
		if done {
			if off, _ := d.Offset(); math.Abs(off) > 0.1 || d.Moving() {
				t.Fatalf("offset %.2f right after the move settled", off)
			}
		}
	}
	if !sawMoving {
		t.Fatal("never moving on the stairs")
	}
}

func TestPressureAltitude(t *testing.T) {
	if h := PressureAltitude(101325); math.Abs(h) > 1e-9 {
		t.Fatalf("sea level = %g", h)
	}
	// 12 Pa per metre near sea level.
	if d := PressureAltitude(101325-42) - PressureAltitude(101325); math.Abs(d-3.5) > 0.05 {
		t.Fatalf("42 Pa = %.3f m, want about 3.5", d)
	}
}
