package sitenav

import (
	"context"
	"math"
	"slices"
)

// Search several nearby basins, retaining competing translations and headings.
// A fit at the search boundary is not evidence that tracking has recovered.
func trackRoom(ctx context.Context, prior PoseCandidate, fit func(PoseCandidate) PoseCandidate) ([]PoseCandidate, error) {
	within := func(c PoseCandidate) bool {
		return c.Position.Distance(prior.Position) <= .5 && math.Abs(math.Remainder(c.Rotation-prior.Rotation, 2*math.Pi)) <= .20
	}
	var seeds []PoseCandidate
	for _, angle := range []float64{-.16, 0, .16} {
		for _, x := range []float64{-.3, 0, .3} {
			for _, y := range []float64{-.3, 0, .3} {
				seeds = append(seeds, fit(PoseCandidate{Position: Point{prior.Position.X + x, prior.Position.Y + y}, Rotation: prior.Rotation + angle}))
			}
		}
	}
	slices.SortStableFunc(seeds, comparePoseCost)
	// Bound CPU on the gateway while keeping distinct nearby explanations.
	seeds = seeds[:8]
	var results []PoseCandidate
	for _, seed := range seeds {
		c := seed
		for _, step := range []struct{ distance, angle float64 }{{.08, .04}, {.025, .012}, {.008, .004}} {
			for iteration := 0; iteration < 10; iteration++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				best := c
				for dx := -1; dx <= 1; dx++ {
					for dy := -1; dy <= 1; dy++ {
						for da := -1; da <= 1; da++ {
							n := PoseCandidate{Position: Point{c.Position.X + float64(dx)*step.distance, c.Position.Y + float64(dy)*step.distance}, Rotation: c.Rotation + float64(da)*step.angle}
							if within(n) {
								n = fit(n)
								if n.cost < best.cost {
									best = n
								}
							}
						}
					}
				}
				if best.cost >= c.cost {
					break
				}
				c = best
			}
		}
		results = append(results, c)
	}
	slices.SortStableFunc(results, comparePoseCost)
	return results, nil
}

// SelectTrackingFit needs broad walls in independent directions and rejects
// near-equal alternatives even when they are only centimetres apart. Furniture
// and saved scans cannot establish an automatic correction on their own.
func SelectTrackingFit(fits []RegistrationFit, prior PoseCandidate) (PoseCandidate, bool) {
	if len(fits) == 0 {
		return PoseCandidate{}, false
	}
	best := fits[0]
	for _, f := range fits {
		if f.Cost < best.Cost {
			best = f
		}
	}
	if !best.Valid || best.NeedsCorroboration || best.Directions < 2 || best.Architecture < 24 || best.Pose.Coverage < .6 || best.Pose.RMS > .14 || best.Pose.Position.Distance(prior.Position) >= .45 || math.Abs(math.Remainder(best.Pose.Rotation-prior.Rotation, 2*math.Pi)) >= .18 {
		return PoseCandidate{}, false
	}
	for _, other := range fits {
		if !other.Plausible {
			continue
		}
		if other.Pose.Position.Distance(best.Pose.Position) > .08 || math.Abs(math.Remainder(other.Pose.Rotation-best.Pose.Rotation, 2*math.Pi)) > .035 {
			if other.Cost-best.Cost <= math.Max(.001, best.Cost*.15) {
				return PoseCandidate{}, false
			}
		}
	}
	return best.Pose, true
}
