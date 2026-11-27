package trackcapture

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// BuildPublication makes a request, not a signature or retained-proof grant.
// The receiving agent performs its existing complete input and archive checks.
func (s Snapshot) BuildPublication(trackID, stream string, retainUntil time.Time) (serviceorigin.PublishRequest, []resultrecord.ValidatedInput, error) {
	c, ok := s.candidates[trackID]
	if !ok || !s.now.Before(c.payload.ValidUntil) || !s.physical.Before(c.payload.PhysicalValidUntil()) {
		return serviceorigin.PublishRequest{}, nil, errors.New("no fresh observed publication for track")
	}
	body, err := json.Marshal(c.payload)
	if err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	r := resultrecord.Result{Version: resultrecord.Version, ResultSchema: ResultSchema, ResultJSON: body, RetainUntilUnixMS: retainUntil.UnixMilli()}
	p := serviceorigin.PublishRequest{Entity: c.payload.RunID + "/track/" + trackID, ComponentType: resultrecord.ComponentType, Stream: stream, Frame: c.payload.Frame, Timing: world.Timing{ClockDomain: c.payload.ClockDomain, ErrorKnown: true}, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: AppVersion, ModelID: AlgorithmID, ModelVersion: AlgorithmVersion}}
	for _, in := range c.inputs {
		r.Inputs = append(r.Inputs, in.Reference)
		p.Timing.SampleTime = minimum(p.Timing.SampleTime, in.Component.Timing.SampleTime)
		leaf := world.Input{ID: in.Reference.ID(), Origin: in.Component.Provenance.Origin}
		if in.Component.Provenance.Simulation != nil {
			v := *in.Component.Provenance.Simulation
			leaf.Simulation = &v
		}
		p.Provenance.Inputs = append(p.Provenance.Inputs, leaf)
	}
	p.TTL = c.payload.ValidUntil.Sub(p.Timing.SampleTime)
	p.Payload, err = resultrecord.Encode(r)
	if err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	if _, err = resultrecord.ValidatePublication(p, c.inputs); err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	if _, err = ValidatePublication(p); err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	// Return owned metadata; signed original envelope bytes remain with caller.
	raw, _ := json.Marshal(c.inputs)
	var inputs []resultrecord.ValidatedInput
	_ = json.Unmarshal(raw, &inputs)
	return p, inputs, nil
}
