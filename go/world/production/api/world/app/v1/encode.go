package appv1

import (
	"bytes"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"time"
)

func encodeOrigin(o world.Origin) Origin {
	switch o {
	case world.Measured:
		return Origin_ORIGIN_MEASURED
	case world.Simulated:
		return Origin_ORIGIN_SIMULATED
	case world.Replayed:
		return Origin_ORIGIN_REPLAYED
	case world.Inferred:
		return Origin_ORIGIN_INFERRED
	case world.Authored:
		return Origin_ORIGIN_AUTHORED
	}
	return Origin_ORIGIN_UNSPECIFIED
}
func encodeSimulation(s *world.SimulationContext) *SimulationContext {
	if s == nil {
		return nil
	}
	return &SimulationContext{RunId: s.RunID, ScenarioVersion: s.ScenarioVersion, Seed: s.Seed, ClockDomain: s.ClockDomain}
}
func encodeTime(t *time.Time) (*int64, error) {
	if t == nil {
		return nil, nil
	}
	ns := t.UnixNano()
	if !time.Unix(0, ns).Equal(*t) {
		return nil, errPublication
	}
	return &ns, nil
}

// EncodePublish is a client helper, not an authorization check. It preserves
// payload bytes and original capture fields; the agent still owns identity.
func EncodePublish(p serviceorigin.PublishRequest) (*PublishRequest, error) {
	if !time.Unix(0, p.Timing.SampleTime.UnixNano()).Equal(p.Timing.SampleTime) || p.TTL <= 0 || p.Timing.ErrorBound < 0 || p.Timing.AgeHint < 0 {
		return nil, errPublication
	}
	captured, e := encodeTime(p.Provenance.OriginalCaptureTime)
	if e != nil {
		return nil, e
	}
	r := &PublishRequest{Entity: p.Entity, ComponentType: p.ComponentType, Stream: p.Stream, Frame: &FrameRef{Id: p.Frame.ID, Revision: p.Frame.Revision}, Timing: &Timing{SampleUnixNs: p.Timing.SampleTime.UnixNano(), ClockDomain: p.Timing.ClockDomain, ErrorKnown: p.Timing.ErrorKnown, ErrorBoundNs: uint64(p.Timing.ErrorBound), AgeHintNs: uint64(p.Timing.AgeHint)}, TtlNs: uint64(p.TTL), PayloadJson: bytes.Clone(p.Payload), Tombstone: p.Tombstone, Provenance: &Provenance{Origin: encodeOrigin(p.Provenance.Origin), AppVersion: p.Provenance.AppVersion, ModelId: p.Provenance.ModelID, ModelVersion: p.Provenance.ModelVersion, Simulation: encodeSimulation(p.Provenance.Simulation), OriginalCaptureUnixNs: captured, OriginalClockDomain: p.Provenance.OriginalClockDomain}}
	for _, in := range p.Provenance.Inputs {
		t, e := encodeTime(in.OriginalCaptureTime)
		if e != nil {
			return nil, e
		}
		r.Provenance.Inputs = append(r.Provenance.Inputs, &Input{Id: in.ID, Origin: encodeOrigin(in.Origin), Simulation: encodeSimulation(in.Simulation), OriginalCaptureUnixNs: t, OriginalClockDomain: in.OriginalClockDomain})
	}
	if _, e := DecodePublish(r); e != nil {
		return nil, e
	}
	return r, nil
}
