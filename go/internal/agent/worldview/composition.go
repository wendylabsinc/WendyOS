package worldview

import (
	"math"
	"sort"
)

// Part is one expected component of a composite object, listed bottom to top.
// Primitive is a solid (one of the Solid constants), or empty to accept any
// silhouette. Size ranges follow SizeExpect: [min, max] metres, [0, 0]
// unconstrained.
type Part struct {
	Primitive string
	WidthM    [2]float64
	HeightM   [2]float64
}

// ObservedPart is one measured component. Primitive is a silhouette (one of
// the Silhouette constants); BottomM is the height of its lower edge, which
// orders the observed parts bottom to top.
type ObservedPart struct {
	Primitive string
	WidthM    float64
	HeightM   float64
	BottomM   float64
}

// partFit is the ScoreSize fit of an observed part against an expected part:
// the minimum over constrained dimensions, 1 when none is constrained.
func partFit(o ObservedPart, p Part) float64 {
	fit := 1.0
	if f, ok := sizeFit(o.WidthM, p.WidthM); ok {
		fit = math.Min(fit, f)
	}
	if f, ok := sizeFit(o.HeightM, p.HeightM); ok {
		fit = math.Min(fit, f)
	}
	return fit
}

// MatchComposition matches observed parts, ordered bottom to top by BottomM,
// against the expected parts as a greedy ordered subsequence: each observed
// part consumes the first remaining expected part, at or after the last one
// matched, whose solid can cast the observed silhouette (see SilhouettesFor)
// and whose size fit is above 0. An "other" silhouette, or an expected
// primitive that is not a known solid, never matches. Expected
// parts skipped over can no longer match, so parts observed out of order do
// not both match. An observed part that matches nothing is ignored.
//
// The score is the sum of the matched parts' fits, each weighted by that
// part's share of the total expected height (range midpoints; equal shares
// when no height is given), so an occluded small part costs only its share.
// visible lists the matched expected part indices in ascending order. With no
// observed parts, no expected parts, or no match, it returns 0, nil.
func MatchComposition(observed []ObservedPart, parts []Part) (score float64, visible []int) {
	if len(observed) == 0 || len(parts) == 0 {
		return 0, nil
	}
	shares := make([]float64, len(parts))
	total := 0.0
	for i, p := range parts {
		lo, hi := ordered(p.HeightM)
		shares[i] = math.Max(0, (lo+hi)/2)
		total += shares[i]
	}
	for i := range shares {
		if total > 0 {
			shares[i] /= total
		} else {
			shares[i] = 1 / float64(len(parts))
		}
	}

	stack := append([]ObservedPart(nil), observed...)
	sort.SliceStable(stack, func(i, j int) bool { return stack[i].BottomM < stack[j].BottomM })

	next := 0
	for _, o := range stack {
		for j := next; j < len(parts); j++ {
			if parts[j].Primitive != "" {
				if compatible, _ := casts(parts[j].Primitive, o.Primitive); !compatible {
					continue
				}
			}
			fit := partFit(o, parts[j])
			if !(fit > 0) {
				continue
			}
			score += fit * shares[j]
			visible = append(visible, j)
			next = j + 1
			break
		}
		if next >= len(parts) {
			break
		}
	}
	if len(visible) == 0 {
		return 0, nil
	}
	return score, visible
}
