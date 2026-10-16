package robotprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// ClockSource reports the device's own wall clock, and how long the round trip to ask
// for it took. The CLI backs it with the agent's clock RPC.
type ClockSource interface {
	// DeviceClock returns the device's clock reading, plus the round-trip time of the
	// call that fetched it.
	DeviceClock(ctx context.Context) (deviceTime time.Time, roundTrip time.Duration, err error)
}

// Clock reports how far the robot's clock sits from this machine's.
//
// It matters more than it looks. Every measured value in the document carries a
// timestamp, frame age is the difference between two clocks, and a recording replayed
// against a log needs both to agree. A robot whose clock is a minute out produces
// measurements that are individually correct and jointly useless.
//
// Drift is only meaningful to within the round trip it took to ask, so the round trip is
// reported as the measurement's uncertainty rather than being hidden.
type Clock struct{}

func (Clock) ID() string                { return "clock" }
func (Clock) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Clock) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementTimeSync}
}
func (Clock) Provides() []string { return []string{"clock.drift", "clock.round_trip"} }

func (p Clock) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementTimeSync)
	if !ok {
		return nil, fmt.Errorf("clock: no timesync handle in the environment")
	}
	source, ok := handle.(ClockSource)
	if !ok {
		return nil, fmt.Errorf("clock: timesync handle is %T, not a ClockSource", handle)
	}

	deviceTime, roundTrip, err := source.DeviceClock(ctx)
	if err != nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
		return []robotinspect.Property{{ID: "clock.drift", Unknown: &unknown}}, nil
	}

	// The device's clock is compared against the midpoint of the request, which is the
	// best estimate of "now" available from one round trip.
	local := time.Now().UTC().Add(-roundTrip / 2)
	drift := deviceTime.Sub(local)
	if drift < 0 {
		drift = -drift
	}

	source_ := robotinspect.Source{Probe: p.ID(), Origin: "agent:timesync"}
	conditions := map[string]string{
		// Naming the uncertainty keeps a sub-millisecond claim honest: a 40 ms round
		// trip cannot support one.
		"round_trip": roundTrip.String(),
		"method":     "single round trip, midpoint estimate",
	}

	driftQuantity, err := robotinspect.NewQuantity(float64(drift.Microseconds())/1000, robotinspect.Milliseconds)
	if err != nil {
		return nil, err
	}
	driftObservation, err := robotinspect.NewObservation(driftQuantity, robotinspect.Measured, source_,
		robotinspect.WithSampling(roundTrip, 1), robotinspect.WithConditions(conditions))
	if err != nil {
		return nil, err
	}

	roundTripQuantity, err := robotinspect.NewQuantity(float64(roundTrip.Microseconds())/1000, robotinspect.Milliseconds)
	if err != nil {
		return nil, err
	}
	roundTripObservation, err := robotinspect.NewObservation(roundTripQuantity, robotinspect.Measured, source_,
		robotinspect.WithSampling(roundTrip, 1))
	if err != nil {
		return nil, err
	}

	return []robotinspect.Property{
		{ID: "clock.drift", Observations: []robotinspect.Observation{driftObservation}},
		{ID: "clock.round_trip", Observations: []robotinspect.Observation{roundTripObservation}},
	}, nil
}
