package robotinspect

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind separates what a robot says about itself from what it was seen doing. A property
// commonly carries several of each, from sources that disagree.
type Kind string

const (
	// Declared came from a manifest, a datasheet, a URDF or the firmware. It is a claim.
	Declared Kind = "declared"
	// Measured came from watching the robot. It carries a sampling window.
	Measured Kind = "measured"
	// Derived was computed from other observations, and names them in its source.
	Derived Kind = "derived"
)

// Source records where an observation came from, precisely enough to go back to it. Probe
// is the registered probe's ID; Origin is the artefact it read, such as
// "topic:/camera/color/camera_info", "urdf:base.urdf" or "datasheet".
type Source struct {
	Probe  string `json:"probe"`
	Origin string `json:"origin"`
}

// Sampling is how a measurement was taken. A measured rate quoted without a window and a
// sample count cannot be reproduced or trusted, so Observation requires this for anything
// Measured.
type Sampling struct {
	Window  time.Duration `json:"windowMs"`
	Samples int           `json:"samples"`
}

// Observation is one answer for one property, with its provenance. Conditions hold the
// state the value depends on — resolution for a field of view, temperature for a travel
// limit — because the same robot gives different answers under different conditions, and
// comparing across them is how a declared number survives contact with a wrong one.
type Observation struct {
	Quantity   Quantity
	Kind       Kind
	Source     Source
	Conditions map[string]string
	Sampling   *Sampling
	ObservedAt time.Time
}

// ObservationOption supplies optional context to NewObservation.
type ObservationOption func(*Observation)

// WithConditions records the state the value is contingent on, such as
// {"resolution": "640x480"}.
func WithConditions(conditions map[string]string) ObservationOption {
	return func(o *Observation) {
		if o.Conditions == nil {
			o.Conditions = make(map[string]string, len(conditions))
		}
		for k, v := range conditions {
			o.Conditions[k] = v
		}
	}
}

// WithSampling records how a measurement was taken. Required for Measured.
func WithSampling(window time.Duration, samples int) ObservationOption {
	return func(o *Observation) { o.Sampling = &Sampling{Window: window, Samples: samples} }
}

// WithObservedAt overrides the observation time, which otherwise defaults to now.
func WithObservedAt(at time.Time) ObservationOption {
	return func(o *Observation) { o.ObservedAt = at }
}

// NewObservation returns an observation, or an error if it is not self-describing: an
// unset quantity, a missing source, or a measurement with no sampling window.
func NewObservation(q Quantity, kind Kind, source Source, opts ...ObservationOption) (Observation, error) {
	o := Observation{Quantity: q, Kind: kind, Source: source, ObservedAt: time.Now().UTC()}
	for _, opt := range opts {
		opt(&o)
	}
	if q.Zero() {
		return Observation{}, fmt.Errorf("robotinspect: observation from %q has no quantity", source.Probe)
	}
	switch kind {
	case Declared, Measured, Derived:
	default:
		return Observation{}, fmt.Errorf("robotinspect: unknown observation kind %q", kind)
	}
	if source.Probe == "" || source.Origin == "" {
		return Observation{}, fmt.Errorf("robotinspect: observation needs both a probe and an origin")
	}
	if kind == Measured {
		if o.Sampling == nil {
			return Observation{}, fmt.Errorf("robotinspect: measured %s from %q needs a sampling window", q, source.Probe)
		}
		if o.Sampling.Window <= 0 || o.Sampling.Samples <= 0 {
			return Observation{}, fmt.Errorf("robotinspect: measured %s from %q has an empty sampling window", q, source.Probe)
		}
	}
	return o, nil
}

// conditionKey renders the conditions in a stable order, so two observations can be
// checked for having been taken under the same circumstances.
func (o Observation) conditionKey() string {
	if len(o.Conditions) == 0 {
		return ""
	}
	keys := make([]string, 0, len(o.Conditions))
	for k := range o.Conditions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", k, o.Conditions[k])
	}
	return b.String()
}
