package robotinspect

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is what the observations for one property add up to.
type Verdict string

const (
	// VerdictAgree: every observation sits within tolerance of the others.
	VerdictAgree Verdict = "agree"
	// VerdictDisagree: comparable observations differ by more than tolerance. This is
	// the finding the command exists to produce.
	VerdictDisagree Verdict = "disagree"
	// VerdictIncomparable: there are several observations but they do not describe the
	// same thing — most often a declared value on one axis against a measured value on
	// another. Calling that a disagreement invites the substitution that produced it,
	// so it gets its own verdict and names both axes.
	VerdictIncomparable Verdict = "incomparable"
	// VerdictSingle: one observation, so there is nothing to reconcile. Not a pass.
	VerdictSingle Verdict = "single"
	// VerdictUnknown: nothing could be observed. Property.Unknown says why.
	VerdictUnknown Verdict = "unknown"
)

// Tolerance is how far comparable observations may sit apart before they disagree. A
// zero Tolerance means any difference at all is a disagreement.
type Tolerance struct {
	Absolute float64 `json:"absolute,omitempty"`
	Relative float64 `json:"relative,omitempty"`
}

// admits reports whether a spread between two values is within tolerance.
func (t Tolerance) admits(low, high float64) bool {
	spread := high - low
	if spread <= t.Absolute {
		return true
	}
	scale := absFloat(high)
	if absFloat(low) > scale {
		scale = absFloat(low)
	}
	return t.Relative > 0 && scale > 0 && spread/scale <= t.Relative
}

// defaultTolerance is what a property gets when it names none. These are deliberately
// tight: a robot whose declared and measured numbers differ by more than this is the
// case worth an operator's attention.
func defaultTolerance(u Unit) Tolerance {
	switch u.symbol {
	case Degrees.symbol, Percent.symbol:
		return Tolerance{Absolute: 1}
	case Radians.symbol:
		return Tolerance{Absolute: 0.02}
	case Millimetres.symbol, Metres.symbol:
		return Tolerance{Absolute: 1, Relative: 0.01}
	case Hertz.symbol:
		return Tolerance{Relative: 0.05}
	case Celsius.symbol:
		return Tolerance{Absolute: 2}
	case Count.symbol, Pixels.symbol, Ticks.symbol:
		// A tally, a pixel dimension and an encoder tick are exact: one more or one
		// fewer is a different answer, not noise.
		return Tolerance{}
	case Bytes.symbol:
		// Two passes over a live filesystem never agree to the byte.
		return Tolerance{Relative: 0.05}
	default:
		return Tolerance{Relative: 0.02}
	}
}

// Property is one fact about a robot and every answer gathered for it. ID is dotted and
// stable, because two inspection documents are diffed by it: "camera.color.fov",
// "joint.waist_yaw.limit", "control.rate".
type Property struct {
	ID           string
	Observations []Observation
	Unknown      *Unknown
	// Tolerance overrides the unit's default. Set it where a property is known to be
	// noisier or tighter than its unit suggests.
	Tolerance *Tolerance
}

// Assessment is a property's verdict with the reasoning an operator needs to act on it.
type Assessment struct {
	Verdict Verdict
	Detail  string
}

// Assess reconciles a property's observations.
func (p Property) Assess() Assessment {
	// A property can hold a claim and still have failed to be measured — asked for a
	// capture mode, then the camera would not start. The claim is worth printing, but
	// the verdict is unknown and must carry the reason, or the reason is lost exactly
	// where it matters.
	if p.Unknown != nil && !observedIn(p.Observations) {
		return Assessment{Verdict: VerdictUnknown, Detail: describeUnknown(p.Unknown)}
	}

	switch len(p.Observations) {
	case 0:
		return Assessment{Verdict: VerdictUnknown, Detail: describeUnknown(p.Unknown)}
	case 1:
		return Assessment{Verdict: VerdictSingle, Detail: string(p.Observations[0].Kind) + " only, never checked against anything"}
	}

	// A property is either textual or numeric throughout. A mix means two sources
	// disagree about what kind of fact this is, which is not a value disagreement.
	texts, quantities := 0, 0
	for _, o := range p.Observations {
		if o.IsText() {
			texts++
		} else {
			quantities++
		}
	}
	if texts > 0 && quantities > 0 {
		return Assessment{Verdict: VerdictIncomparable,
			Detail: "not the same kind of fact: some sources report text and others a quantity"}
	}
	if texts > 0 {
		return assessText(p.Observations)
	}

	groups := p.groupByComparability()
	if len(groups) > 1 {
		return Assessment{Verdict: VerdictIncomparable, Detail: describeGroups(groups)}
	}

	tolerance := p.tolerance()
	low, high := p.Observations[0], p.Observations[0]
	for _, o := range p.Observations[1:] {
		if o.Quantity.value < low.Quantity.value {
			low = o
		}
		if o.Quantity.value > high.Quantity.value {
			high = o
		}
	}
	if tolerance.admits(low.Quantity.value, high.Quantity.value) {
		return Assessment{Verdict: VerdictAgree}
	}
	return Assessment{Verdict: VerdictDisagree, Detail: describeSpread(low, high)}
}

func describeUnknown(unknown *Unknown) string {
	if unknown == nil {
		return ""
	}
	if unknown.Detail == "" {
		return unknown.Reason
	}
	return unknown.Reason + ": " + unknown.Detail
}

// assessText reconciles textual observations. There is no tolerance for a firmware
// version or a board model: either every source says the same thing or they do not.
func assessText(observations []Observation) Assessment {
	first := observations[0]
	for _, o := range observations[1:] {
		if o.Text != first.Text {
			return Assessment{Verdict: VerdictDisagree,
				Detail: fmt.Sprintf("%s %q vs %s %q", first.Kind, first.Text, o.Kind, o.Text)}
		}
	}
	return Assessment{Verdict: VerdictAgree}
}

// groupByComparability buckets observations that describe the same thing. The key
// deliberately excludes Conditions: a declared value carries none and a measured one
// carries the resolution or temperature it was taken at, and comparing across that is
// the entire point.
func (p Property) groupByComparability() map[string][]Observation {
	groups := map[string][]Observation{}
	for _, o := range p.Observations {
		q := o.Quantity
		key := q.unit.symbol + "|" + q.axis + "|" + q.frame
		groups[key] = append(groups[key], o)
	}
	return groups
}

func (p Property) tolerance() Tolerance {
	if p.Tolerance != nil {
		return *p.Tolerance
	}
	if len(p.Observations) == 0 {
		return Tolerance{}
	}
	return defaultTolerance(p.Observations[0].Quantity.unit)
}

// describeGroups names what makes the observations incomparable, in the operator's
// terms: which kind was recorded on which axis.
func describeGroups(groups map[string][]Observation) string {
	descriptions := make([]string, 0, len(groups))
	for _, observations := range groups {
		kinds := map[Kind]bool{}
		for _, o := range observations {
			kinds[o.Kind] = true
		}
		names := make([]string, 0, len(kinds))
		for k := range kinds {
			names = append(names, string(k))
		}
		sort.Strings(names)
		q := observations[0].Quantity
		qualifier := q.axis
		if q.frame != "" {
			qualifier = strings.TrimSpace(qualifier + " in " + q.frame)
		}
		if qualifier == "" {
			qualifier = q.unit.orName()
		}
		descriptions = append(descriptions, fmt.Sprintf("%s is %s", strings.Join(names, "/"), qualifier))
	}
	sort.Strings(descriptions)
	return "not the same measurement: " + strings.Join(descriptions, ", ")
}

func describeSpread(low, high Observation) string {
	detail := fmt.Sprintf("%s %s vs %s %s", low.Kind, low.Quantity, high.Kind, high.Quantity)
	if lowConditions, highConditions := low.conditionKey(), high.conditionKey(); lowConditions != highConditions {
		switch {
		case lowConditions == "":
			detail += fmt.Sprintf(" (%s taken at %s)", high.Kind, highConditions)
		case highConditions == "":
			detail += fmt.Sprintf(" (%s taken at %s)", low.Kind, lowConditions)
		default:
			detail += fmt.Sprintf(" (%s, %s)", lowConditions, highConditions)
		}
	}
	return detail
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
