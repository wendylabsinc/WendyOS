package worldview

import "sort"

// DefaultRequiredMin is the veto floor for a required attribute whose floor
// FusionSpec.Min does not set.
const DefaultRequiredMin = 0.5

// FusionSpec says how per-attribute evidence combines into one confidence.
type FusionSpec struct {
	Threshold float64
	Required  []string
	Weights   map[string]float64 // per attribute, > 0; a missing or non-positive weight counts as 1
	Min       map[string]float64 // veto floor for required attributes; default 0.5
}

// Fused is the combined verdict on one proposal.
type Fused struct {
	Confidence  float64
	Scores      map[string]float64 // available attributes only
	Unavailable []string           // sorted
	Vetoed      string             // attribute name, or ""
	Matched     bool
}

// Fuse combines evidence into a weighted mean whose weights are renormalised
// over the available attributes only, so an unavailable attribute neither
// raises nor lowers the confidence. A required attribute that is unavailable
// (or absent from evidence altogether), or that scores below its floor, vetoes
// the match; required attributes are checked in sorted order and the first
// failure is reported. When an attribute appears more than once the first
// available entry is used. Scores are clamped to [0, 1], NaN counting as 0.
func Fuse(evidence []Evidence, spec FusionSpec) Fused {
	out := Fused{Scores: map[string]float64{}}
	seen := map[string]bool{}
	for _, e := range evidence {
		seen[e.Attribute] = true
		if !e.Available {
			continue
		}
		if _, dup := out.Scores[e.Attribute]; dup {
			continue
		}
		out.Scores[e.Attribute] = clamp01(e.Score)
	}
	unavailable := map[string]bool{}
	for name := range seen {
		if _, ok := out.Scores[name]; !ok {
			unavailable[name] = true
		}
	}
	for _, name := range spec.Required {
		if _, ok := out.Scores[name]; !ok {
			unavailable[name] = true
		}
	}
	out.Unavailable = sortedKeys(unavailable)

	names := sortedKeys(out.Scores)
	sum, weights := 0.0, 0.0
	for _, name := range names {
		w := spec.Weights[name]
		if !(w > 0) {
			w = 1
		}
		sum += w * out.Scores[name]
		weights += w
	}
	if weights > 0 {
		out.Confidence = sum / weights
	}

	required := append([]string(nil), spec.Required...)
	sort.Strings(required)
	for _, name := range required {
		score, ok := out.Scores[name]
		floor, set := spec.Min[name]
		if !set {
			floor = DefaultRequiredMin
		}
		if !ok || score < floor {
			out.Vetoed = name
			break
		}
	}
	out.Matched = len(out.Scores) > 0 && out.Vetoed == "" && out.Confidence >= spec.Threshold
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
