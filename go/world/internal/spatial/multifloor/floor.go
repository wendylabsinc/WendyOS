package multifloor

import (
	"math"
	"slices"
	"time"
)

// FloorConfig tunes the barometric floor-change detector. A zero field takes
// its default.
type FloorConfig struct {
	// FilterWindow is the span of the running median over the raw heights. A
	// pressure spike shorter than half of it (a slammed door, a gust) does not
	// pass. Default 2 s.
	FilterWindow time.Duration
	// Dwell is how long the filtered height must hold still before a level is
	// accepted. Default 3 s.
	Dwell time.Duration
	// SettleBandM is the largest spread of the filtered height over Dwell that
	// still counts as holding still. Default 0.5 m.
	SettleBandM float64
	// LevelBandM is how far the filtered height may be from the baseline of
	// the current floor before the device counts as between floors. Default
	// 0.4 m.
	LevelBandM float64
	// MinFloorHeightM is the smallest settled height change that is a floor
	// change: lower than any storey, higher than a ventilation pressure step
	// or a half landing. Default 2 m.
	MinFloorHeightM float64
	// DriftRateMPS bounds how fast the baseline follows the filtered height
	// while it holds still. Weather and sensor drift below this rate are
	// absorbed; anything faster is motion. Default 0.02 m/s (a passing front
	// moves a barometer by about 0.01 m/s at worst).
	DriftRateMPS float64
	// StillRateMPS is the vertical rate below which a settled height counts
	// as standing still: its mean over one Dwell is within StillRateMPS·Dwell
	// of its mean over the Dwell before. Only a still height is followed as
	// drift, and a height settled a storey away is a floor only once it is
	// still or has turned back. It must be above DriftRateMPS. The two means
	// differ by chance by about sigma·√(2/samples per Dwell), a third of the
	// default threshold with a 0.1 m barometer at 5 Hz, so climbs at twice
	// this rate and faster are reliably seen whole. Default 0.05 m/s.
	StillRateMPS float64
	// MaxGap is the longest silence the baseline survives. After a longer one
	// the drift over the gap is unbounded and the baseline is learned again.
	// Default 10 s.
	MaxGap time.Duration
	// History is how far back the start of a move is searched. A climb that
	// takes longer keeps its end and loses its start. Default 120 s.
	History time.Duration
}

func (c FloorConfig) withDefaults() FloorConfig {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	dur(&c.FilterWindow, 2*time.Second)
	dur(&c.Dwell, 3*time.Second)
	dur(&c.MaxGap, 10*time.Second)
	dur(&c.History, 120*time.Second)
	def(&c.SettleBandM, 0.5)
	def(&c.LevelBandM, 0.4)
	def(&c.MinFloorHeightM, 2)
	def(&c.DriftRateMPS, 0.02)
	def(&c.StillRateMPS, 0.05)
	return c
}

// PressureAltitude is the altitude in metres at which the international
// standard atmosphere has the given pressure in pascals. Its zero is
// meaningless here; differences over seconds are heights.
//
// ponytail: standard-atmosphere temperature, so a height difference is off
// by the ratio of the real air temperature to 288 K (about 5 % at 0 °C or
// 30 °C); scale by a measured temperature if storeys must be told apart to
// better than that.
func PressureAltitude(pa float64) float64 {
	return 44330.77 * (1 - math.Pow(pa/101325, 0.190263))
}

// Move is one settled change of height.
type Move struct {
	// Start and End bound the vertical motion. Mid is the instant the device
	// was half way, which is when it changed floor. All three are back-dated
	// from Confirmed, the instant the detector accepted the new level.
	Start, Mid, End, Confirmed time.Time
	// DeltaM is the height gained, negative going down. SigmaM is a
	// conservative bound on its error: the noise of the two levels plus the
	// drift the baseline could not follow during the move.
	DeltaM, SigmaM float64
}

type sample struct {
	t time.Time
	h float64
}

// Detector finds floor changes in one device's barometer stream.
//
// The raw height passes a running median. While the filtered height holds
// still within a storey of the level's baseline, the baseline follows it at no
// more than DriftRateMPS. A height that settles at least MinFloorHeightM away
// is a candidate level, and the candidate is a Move once the device is known
// to have stopped there: its height has stopped changing (StillRateMPS), or it
// has turned back. A spike never settles away from the baseline, slow drift is
// followed, a pressure step smaller than a storey is absorbed into the
// baseline, and a climb too slow to leave the settle band is not cut short
// half way, so none of them is a wrong Move.
//
// ponytail: a level visited for less than Dwell (up and straight back down)
// is never seen; shorten Dwell for devices that do that. A climb slower than
// twice StillRateMPS (0.1 m/s: a 1:12 ramp at a slow walk) is cut short at
// MinFloorHeightM or partly absorbed as drift; telling it from weather needs
// a longer Dwell or the odometer, which the Mapper has and this detector does
// not use.
type Detector struct {
	cfg  FloorConfig
	raw  []sample // the last FilterWindow of raw heights
	hist []sample // the last History of filtered heights
	ref  float64  // baseline of the current level
	has  bool
	last time.Time

	// The candidate level: settled a storey from ref, not yet known to be
	// where the device stopped.
	away       bool
	cand       sample
	candSpread float64
}

// NewDetector returns a detector with cfg's zero fields defaulted.
func NewDetector(cfg FloorConfig) *Detector { return &Detector{cfg: cfg.withDefaults()} }

// Add takes the next height sample (metres, arbitrary zero: see
// PressureAltitude) with its 1σ noise and reports a Move when one has just
// been confirmed. Samples must arrive in time order; one that is not is
// ignored.
func (d *Detector) Add(t time.Time, height, sigma float64) (Move, bool) {
	if !d.last.IsZero() {
		if !t.After(d.last) {
			return Move{}, false
		}
		if t.Sub(d.last) > d.cfg.MaxGap {
			d.raw, d.hist, d.has, d.away = d.raw[:0], d.hist[:0], false, false
		}
	}
	d.last = t
	d.raw = trim(append(d.raw, sample{t, height}), t.Add(-d.cfg.FilterWindow))
	// The median of a window over a ramp is the height at the window's
	// centre, so that is the instant the filtered sample belongs to.
	now := sample{d.raw[0].t.Add(t.Sub(d.raw[0].t) / 2), median(d.raw)}
	step := 0.0
	if n := len(d.hist); n > 0 {
		step = d.cfg.DriftRateMPS * now.t.Sub(d.hist[n-1].t).Seconds()
	}
	d.hist = trim(append(d.hist, now), now.t.Add(-d.cfg.History))
	noise := sigma / math.Sqrt(float64(len(d.raw)))

	// Turned back from the candidate: that was where it stopped.
	if d.away && (now.h-d.cand.h)*(d.cand.h-d.ref) < -d.cfg.LevelBandM*math.Abs(d.cand.h-d.ref) {
		return d.arrive(t, noise), true
	}
	from := now.t.Add(-d.cfg.Dwell)
	if d.hist[0].t.After(from) {
		return Move{}, false
	}
	level, spread, band, ok := d.window(from, now.t)
	if !ok || band > d.cfg.SettleBandM {
		return Move{}, false
	}
	if !d.has {
		d.ref, d.has = level, true
		return Move{}, false
	}
	// Still: the level is where it was one Dwell ago. Only a level that is
	// still is followed as drift or accepted as a floor; one that creeps is a
	// slow climb under way.
	still := true
	if before, _, _, ok := d.window(from.Add(-d.cfg.Dwell), from); ok {
		still = math.Abs(level-before) <= d.cfg.StillRateMPS*d.cfg.Dwell.Seconds()
	}
	off := level - d.ref
	if math.Abs(off) < d.cfg.MinFloorHeightM {
		if d.away = false; still {
			d.ref += math.Max(-step, math.Min(step, off))
		}
		return Move{}, false
	}
	d.away, d.cand, d.candSpread = true, sample{now.t, level}, spread
	if still {
		return d.arrive(t, noise), true
	}
	return Move{}, false
}

// window is the mean, the standard deviation and the range of the filtered
// heights in (from, to]; ok is false when there are none.
func (d *Detector) window(from, to time.Time) (mean, spread, band float64, ok bool) {
	lo, hi, sum, sum2, n := math.Inf(1), math.Inf(-1), 0.0, 0.0, 0.0
	for i := len(d.hist) - 1; i >= 0 && d.hist[i].t.After(from); i-- {
		if h := d.hist[i].h; !d.hist[i].t.After(to) {
			lo, hi, sum, sum2, n = math.Min(lo, h), math.Max(hi, h), sum+h, sum2+h*h, n+1
		}
	}
	if n == 0 {
		return 0, 0, 0, false
	}
	mean = sum / n
	return mean, math.Sqrt(math.Max(sum2/n-mean*mean, 0)), hi - lo, true
}

// arrive makes the candidate the current level and returns the Move to it.
func (d *Detector) arrive(confirmed time.Time, noise float64) Move {
	mv := d.move(d.cand.h-d.ref, d.cand.t, confirmed)
	mv.SigmaM = math.Sqrt2*math.Max(d.candSpread, noise) + d.cfg.DriftRateMPS*mv.End.Sub(mv.Start).Seconds()
	d.ref, d.away = d.cand.h, false
	return mv
}

// move back-dates a height change of delta, settled at the given instant,
// from the filtered history.
// The instants the height passed a quarter, half and three quarters of the way
// are read off the history; the start and the end are extrapolated from the
// quarter points, which a constant climb rate makes exact and which noise near
// the two levels does not disturb.
func (d *Detector) move(delta float64, settled, confirmed time.Time) Move {
	frac := func(i int) float64 { return (d.hist[i].h - d.ref) / delta }
	// cross is the last instant at or before index hi at which the height
	// passed fraction f on its way to the new level, and the index below it.
	cross := func(f float64, hi int) (time.Time, int) {
		for i := hi; i >= 0; i-- {
			a := frac(i)
			if a >= f {
				continue
			}
			if i+1 == len(d.hist) {
				return d.hist[i].t, i
			}
			u := (f - a) / (frac(i+1) - a)
			return d.hist[i].t.Add(time.Duration(u * float64(d.hist[i+1].t.Sub(d.hist[i].t)))), i
		}
		return d.hist[0].t, 0
	}
	last := len(d.hist) - 1
	for last > 0 && d.hist[last].t.After(settled) {
		last--
	}
	t75, i := cross(0.75, last)
	t50, j := cross(0.5, i)
	t25, _ := cross(0.25, j)
	half := t75.Sub(t25) / 2
	mv := Move{Start: t25.Add(-half), Mid: t50, End: t75.Add(half), Confirmed: confirmed, DeltaM: delta}
	if mv.Start.Before(d.hist[0].t) {
		mv.Start = d.hist[0].t
	}
	if mv.End.After(confirmed) {
		mv.End = confirmed
	}
	return mv
}

// Offset is the filtered height above the baseline of the current level. It
// is false until a level has been learned.
func (d *Detector) Offset() (float64, bool) {
	if !d.has || len(d.hist) == 0 {
		return 0, false
	}
	return d.hist[len(d.hist)-1].h - d.ref, true
}

// Moving reports whether the device is away from its level: on the stairs, in
// a moving lift, or under a pressure disturbance that has not yet resolved.
func (d *Detector) Moving() bool {
	off, ok := d.Offset()
	return ok && math.Abs(off) > d.cfg.LevelBandM
}

// trim drops the samples at or before cutoff from the front, in place.
func trim(s []sample, cutoff time.Time) []sample {
	i := 0
	for i < len(s)-1 && !s[i].t.After(cutoff) {
		i++
	}
	return s[:copy(s, s[i:])]
}

func median(s []sample) float64 {
	h := make([]float64, len(s))
	for i := range s {
		h[i] = s[i].h
	}
	slices.Sort(h)
	if n := len(h); n%2 == 1 {
		return h[n/2]
	} else {
		return (h[n/2-1] + h[n/2]) / 2
	}
}
