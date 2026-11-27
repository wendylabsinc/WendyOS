package sensorrecord

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// LiveDeadlines derives immutable interpretation bounds from a validated raw
// capture. It does not establish proof authority or current time. Callers must
// shorten physical validity for certificates, policy and retained media, and
// check both returned deadlines after their last blocking work.
//
// Age hints shorten from original sample time, never from lookup/receipt time:
// repeated historical reads cannot renew a hint-based lifetime.
func LiveDeadlines(component world.Component) (sampleUntil, physicalUntil time.Time, err error) {
	capture, err := FromComponent(component)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	sample, err := capture.Sample()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	margin := max(component.Timing.ErrorBound, component.Timing.AgeHint)
	if margin >= component.TTL || capture.CapturedUnixNS > math.MaxInt64-int64(MaxPhysicalAge) {
		return time.Time{}, time.Time{}, errors.New("capture has no bounded remaining lifetime")
	}
	span := time.Duration(0)
	if sample.Audio != nil {
		span = time.Duration(sample.Audio.DurationNS)
	}
	if span >= component.TTL-margin {
		return time.Time{}, time.Time{}, errors.New("complete audio interval has expired")
	}
	sampleUntil = component.Timing.SampleTime.Add(component.TTL - margin - span)
	if sample.Camera != nil {
		valid := time.Duration(sample.Camera.CalibrationValidForNS)
		if valid <= margin {
			return time.Time{}, time.Time{}, errors.New("camera calibration has expired")
		}
		calibrationUntil := component.Timing.SampleTime.Add(valid - margin)
		if calibrationUntil.Before(sampleUntil) {
			sampleUntil = calibrationUntil
		}
	}
	physicalUntil = time.Unix(0, capture.CapturedUnixNS).Add(MaxPhysicalAge)
	return sampleUntil, physicalUntil, nil
}
