package resultrecord

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func validateInput(input ValidatedInput) error {
	c := input.Component
	if input.Reference.Validate() != nil || input.Reference.Key != c.Key || input.Reference.Version != c.Version || c.Tombstone || c.Key.Type == ComponentType || c.Frame.Validate() != nil || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || !validTime(c.Timing.SampleTime) || c.TTL <= 0 || c.TTL > MaxLifetime || c.Timing.ErrorBound >= c.TTL || c.Timing.AgeHint >= c.TTL-c.Timing.ErrorBound || c.Provenance.Validate() != nil {
		return errors.New("validated input disagrees with exact raw source component")
	}
	if !validTime(input.SampleValidUntil) || !validTime(input.PhysicalValidUntil) || !input.SampleValidUntil.After(c.Timing.SampleTime) || input.SampleValidUntil.After(c.Timing.SampleTime.Add(c.TTL-c.Timing.ErrorBound)) {
		return errors.New("validated input lacks bounded effective source and physical deadlines")
	}
	switch c.Provenance.Origin {
	case world.Measured:
		if strings.HasPrefix(c.Timing.ClockDomain, "simulation:") || strings.HasPrefix(c.Timing.ClockDomain, "replay:") {
			return errors.New("measured leaf cannot use a synthetic source clock")
		}
	case world.Simulated:
		if c.Provenance.Simulation.ClockDomain != c.Timing.ClockDomain || c.Timing.ClockDomain != "simulation:"+c.Provenance.Simulation.RunID {
			return errors.New("raw simulation ancestry and source clock disagree")
		}
	default:
		return errors.New("derived inputs require original measured or simulated leaves")
	}
	return nil
}

// ValidatePublication matches every app-supplied reference and provenance leaf
// to exact verified host inputs. The host input list may be reordered, but must
// have no missing, extra or duplicate entries. The outer provenance list follows
// Result.Inputs order. No reference, timestamp or JSON field grants authority.
//
// This first contract admits homogeneous measured or simulated raw inputs in
// one tenant/site/frame/clock domain. Replay, derived-input recursion and mixed
// ancestry need separate contracts. Retention permission is deliberately not
// inferred from live input deadlines and must be enforced by the host archive.
func ValidatePublication(p serviceorigin.PublishRequest, inputs []ValidatedInput) (Validation, error) {
	r, err := Decode(p.Payload)
	if err != nil {
		return Validation{}, err
	}
	if p.ComponentType != ComponentType || p.Tombstone || !exactText(p.Entity, 256) || !exactText(p.Stream, 256) || p.Frame.Validate() != nil || p.Timing.Validate() != nil || !p.Timing.ErrorKnown || !validTime(p.Timing.SampleTime) || p.TTL <= 0 || p.TTL > MaxLifetime || p.Timing.ErrorBound >= p.TTL || p.Timing.AgeHint >= p.TTL-p.Timing.ErrorBound || p.Provenance.Validate() != nil || p.Provenance.Origin != world.Inferred || !exactText(p.Provenance.AppVersion, 256) || !exactText(p.Provenance.ModelID, 256) || !exactText(p.Provenance.ModelVersion, 256) || len(inputs) != len(r.Inputs) || len(p.Provenance.Inputs) != len(r.Inputs) {
		return Validation{}, errors.New("invalid derived publication metadata or input count")
	}
	verified := make(map[string]ValidatedInput, len(inputs))
	for _, input := range inputs {
		if err := validateInput(input); err != nil {
			return Validation{}, err
		}
		key := input.Reference.EnvelopeSHA256
		if _, duplicate := verified[key]; duplicate {
			return Validation{}, errors.New("duplicate host-validated input")
		}
		verified[key] = input
	}
	var first *world.Component
	var sourceUntil, physicalUntil time.Time
	for i, ref := range r.Inputs {
		input, ok := verified[ref.EnvelopeSHA256]
		if !ok || input.Reference != ref {
			return Validation{}, errors.New("derived reference differs from verified proof hash, key or version")
		}
		c := input.Component
		if c.Frame != p.Frame || c.Timing.ClockDomain != p.Timing.ClockDomain {
			return Validation{}, errors.New("derived input frame or source clock mismatch")
		}
		if first == nil {
			copy := c
			first = &copy
		} else if c.Key.Tenant != first.Key.Tenant || c.Key.Site != first.Key.Site || c.Provenance.Origin != first.Provenance.Origin || !reflect.DeepEqual(c.Provenance.Simulation, first.Provenance.Simulation) {
			return Validation{}, errors.New("derived inputs mix tenant, site or source ancestry")
		}
		leaf := world.Input{ID: ref.ID(), Origin: c.Provenance.Origin, Simulation: c.Provenance.Simulation}
		if !reflect.DeepEqual(p.Provenance.Inputs[i], leaf) {
			return Validation{}, errors.New("derived provenance rewrites or omits an exact raw source leaf")
		}
		if sourceUntil.IsZero() || input.SampleValidUntil.Before(sourceUntil) {
			sourceUntil = input.SampleValidUntil
		}
		if physicalUntil.IsZero() || input.PhysicalValidUntil.Before(physicalUntil) {
			physicalUntil = input.PhysicalValidUntil
		}
	}
	// Neither increasing uncertainty nor a relayed age hint can justify a
	// longer output TTL. Bound even the latest possible nominal end by every
	// input's already-conservative source deadline.
	nominalEnd := p.Timing.SampleTime.Add(p.TTL)
	latestEnd := nominalEnd.Add(p.Timing.ErrorBound)
	if !validTime(latestEnd) || latestEnd.After(sourceUntil) {
		return Validation{}, errors.New("derived lifetime exceeds a required input deadline")
	}
	return Validation{Result: r, SampleValidUntil: nominalEnd.Add(-p.Timing.ErrorBound), PhysicalValidUntil: physicalUntil}, nil
}
