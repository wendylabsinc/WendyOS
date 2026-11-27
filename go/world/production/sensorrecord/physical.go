package sensorrecord

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// PhysicalCurrent bounds the original wall-capture assertion against one
// independently authenticated physical interval. Uncertainty consumes the age
// budget. It neither authenticates the sample nor provides a simulation clock.
// Batch callers must use one final interval for every returned capture after
// all blocking work; per-record checks alone can let an earlier result expire.
func PhysicalCurrent(c Capture, interval trustedtime.Interval) error {
	if c.CapturedUnixNS <= 0 || interval.Validate() != nil {
		return errors.New("capture requires authenticated physical time")
	}
	captured := time.Unix(0, c.CapturedUnixNS)
	if captured.After(interval.Latest) || interval.Latest.Sub(captured) > MaxPhysicalAge {
		return errors.New("capture exceeds physical age bound")
	}
	return nil
}
