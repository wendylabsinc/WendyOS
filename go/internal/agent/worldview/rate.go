package worldview

import (
	"sync"
	"time"
)

// RateWindow is the sliding window RateMeter averages over.
const RateWindow = 5 * time.Second

// RateMeter measures the achieved frame rate over a sliding RateWindow so a
// service can record requested versus achieved rate. Ticks must arrive in
// non-decreasing time order. The zero value is ready to use and it is safe for
// concurrent use.
type RateMeter struct {
	mu    sync.Mutex
	first time.Time
	ticks []time.Time
}

// Tick records one frame at now.
func (r *RateMeter) Tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.first.IsZero() {
		r.first = now
	}
	r.ticks = append(r.ticks, now)
	r.prune(now)
}

// Achieved returns frames per second over the RateWindow ending at now. Once
// the meter has run for a full window it is the number of ticks in
// (now - RateWindow, now] divided by the window. Before that it is the number
// of intervals between ticks divided by the time since the first tick, so a
// steady stream reads its true rate from the second tick on. With fewer than
// two ticks ever, it is 0.
func (r *RateMeter) Achieved(now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(now)
	if r.first.IsZero() {
		return 0
	}
	elapsed := now.Sub(r.first)
	if elapsed >= RateWindow {
		return float64(len(r.ticks)) / RateWindow.Seconds()
	}
	if elapsed <= 0 || len(r.ticks) < 2 {
		return 0
	}
	return float64(len(r.ticks)-1) / elapsed.Seconds()
}

func (r *RateMeter) prune(now time.Time) {
	cutoff := now.Add(-RateWindow)
	drop := 0
	for drop < len(r.ticks) && !r.ticks[drop].After(cutoff) {
		drop++
	}
	if drop > 0 {
		r.ticks = append(r.ticks[:0], r.ticks[drop:]...)
	}
}
