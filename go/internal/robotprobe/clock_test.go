package robotprobe

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeClock struct {
	offset    time.Duration
	roundTrip time.Duration
	err       error
}

func (f fakeClock) DeviceClock(context.Context) (time.Time, time.Duration, error) {
	if f.err != nil {
		return time.Time{}, 0, f.err
	}
	return time.Now().UTC().Add(f.offset), f.roundTrip, nil
}

func clockEnv(source ClockSource) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementTimeSync, source)
}

func TestClockReportsDriftAndItsUncertainty(t *testing.T) {
	properties, err := Clock{}.Observe(context.Background(),
		clockEnv(fakeClock{offset: 40 * time.Millisecond, roundTrip: 8 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}

	drift := findIn(t, properties, "clock.drift")
	observation := drift.Observations[0]
	// The midpoint estimate puts the device roughly 40ms ahead, minus half the trip.
	if got := observation.Quantity.Value(); math.Abs(got-36) > 8 {
		t.Errorf("drift = %.1f ms, want about 36", got)
	}
	if observation.Kind != robotinspect.Measured {
		t.Errorf("kind = %q, want measured", observation.Kind)
	}
	// A sub-millisecond claim cannot be supported by an 8ms round trip, so the round
	// trip travels with the value rather than being hidden.
	if got := observation.Conditions["round_trip"]; !strings.Contains(got, "ms") {
		t.Errorf("round_trip condition = %q, want the round trip named", got)
	}
	if observation.Sampling == nil || observation.Sampling.Samples != 1 {
		t.Errorf("sampling = %+v, want one sample", observation.Sampling)
	}

	if got := findIn(t, properties, "clock.round_trip").Observations[0].Quantity.Value(); math.Abs(got-8) > 1 {
		t.Errorf("round_trip = %.1f ms, want 8", got)
	}
}

// Drift is reported as a magnitude: a robot 40ms behind is as wrong as one 40ms ahead.
func TestClockReportsDriftAsAMagnitude(t *testing.T) {
	for _, offset := range []time.Duration{50 * time.Millisecond, -50 * time.Millisecond} {
		properties, err := Clock{}.Observe(context.Background(),
			clockEnv(fakeClock{offset: offset, roundTrip: 2 * time.Millisecond}))
		if err != nil {
			t.Fatal(err)
		}
		if got := findIn(t, properties, "clock.drift").Observations[0].Quantity.Value(); got < 0 {
			t.Errorf("drift for offset %s = %v, want a magnitude", offset, got)
		}
	}
}

func TestClockReportsAFailureAsUnknown(t *testing.T) {
	properties, err := Clock{}.Observe(context.Background(),
		clockEnv(fakeClock{err: errors.New("timesync unimplemented on this agent")}))
	if err != nil {
		t.Fatalf("an older agent without the RPC is a finding, not a failure: %v", err)
	}
	drift := findIn(t, properties, "clock.drift")
	if drift.Unknown == nil || drift.Unknown.Reason != robotinspect.ReasonProbeFailed {
		t.Errorf("drift = %+v, want an unknown", drift.Unknown)
	}
}

func TestClockRefusesAnEnvironmentWithoutTimeSync(t *testing.T) {
	if _, err := (Clock{}).Observe(context.Background(), robotinspect.NewEnv()); err == nil {
		t.Error("the probe ran with no timesync handle")
	}
	wrong := robotinspect.NewEnv().Offer(robotinspect.RequirementTimeSync, "not a source")
	if _, err := (Clock{}).Observe(context.Background(), wrong); err == nil {
		t.Error("the probe accepted a handle that is not a ClockSource")
	}
}
