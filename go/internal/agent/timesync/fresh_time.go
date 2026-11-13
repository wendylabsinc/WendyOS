package timesync

import (
	"context"
	"fmt"
	"math"
	"time"
)

// TimeWindow bounds current time using a fresh authenticated consensus. A
// time-gated operation must be valid throughout it, not just at its midpoint.
type TimeWindow struct {
	Earliest time.Time
	Latest   time.Time
	// Boot is the CLOCK_BOOTTIME sample the bounds were anchored to.
	Boot int64
}

// BootTime reads the suspend-inclusive clock that anchors TimeWindow.Boot.
func BootTime() (int64, error) { return bootTimeNanos() }

// Advance moves the window by the boot time elapsed since it was sampled.
// Unlike time.Since (CLOCK_MONOTONIC), CLOCK_BOOTTIME keeps counting through
// suspend, so a window held across I/O cannot lag real time.
func (w TimeWindow) Advance() (TimeWindow, error) {
	now, err := bootTimeNanos()
	if err != nil {
		return TimeWindow{}, fmt.Errorf("timesync: boot clock unavailable: %w", err)
	}
	if w.Boot <= 0 || now < w.Boot {
		return TimeWindow{}, fmt.Errorf("timesync: window has no boot-time anchor")
	}
	elapsed := time.Duration(now - w.Boot)
	return TimeWindow{Earliest: w.Earliest.Add(elapsed), Latest: w.Latest.Add(elapsed), Boot: now}, nil
}

// FreshTime obtains new nonce-bound signed responses from the existing baked-in
// servers. It neither reads/writes clock_floor nor disciplines the system clock:
// boot continuity and persistence remain the existing time services' job.
func (m *Manager) FreshTime(ctx context.Context) (TimeWindow, error) {
	queryCtx, cancel := context.WithTimeout(ctx, maxCaptureRTT)
	defer cancel()
	c, err := m.queryConsensus(queryCtx, Servers)
	if err != nil {
		return TimeWindow{}, fmt.Errorf("timesync: fresh authenticated query failed: %w", err)
	}
	// QueryConsensus can collect a valid quorum before an unreachable server
	// exhausts the collection deadline. Included responses already passed the
	// RTT bound, and their offsets remain anchored to boot time. Preserve that
	// quorum unless the caller canceled the operation.
	if err := ctx.Err(); err != nil {
		return TimeWindow{}, err
	}
	if !disciplinesClock(c) || c.Quorum < 2 || c.UpperOffsetNanos < c.LowerOffsetNanos {
		return TimeWindow{}, fmt.Errorf("timesync: fresh authenticated quorum required")
	}
	boot, err := bootTimeNanos()
	if err != nil {
		return TimeWindow{}, err
	}
	if boot < 0 || c.UpperOffsetNanos > math.MaxInt64-boot {
		return TimeWindow{}, fmt.Errorf("timesync: authenticated upper bound overflow")
	}
	window := TimeWindow{
		Earliest: time.Unix(0, boot+c.LowerOffsetNanos),
		Latest:   time.Unix(0, boot+c.UpperOffsetNanos),
		Boot:     boot,
	}
	if window.Earliest.Before(floorMin) || window.Latest.After(floorMax) {
		return TimeWindow{}, fmt.Errorf("timesync: implausible authenticated interval")
	}
	return window, nil
}
