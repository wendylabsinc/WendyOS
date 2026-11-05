package backend

import (
	"math"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
)

// The destination is 1.5 m from the wearer. A separate 1 m exclusion radius
// constrains every route segment and braking horizon, not just the endpoint.
// Existing footprint, obstacle and floor checks remain mandatory as well.
const personalSpaceRadius = 1.0

func personalSpaceClear(center *sitenav.Point, path []sitenav.Point) bool {
	if center == nil {
		return true
	}
	if !center.Valid() || len(path) == 0 {
		return false
	}
	for i, p := range path {
		if !p.Valid() || p.Distance(*center) < personalSpaceRadius {
			return false
		}
		if i == 0 {
			continue
		}
		a := path[i-1]
		dx, dy := p.X-a.X, p.Y-a.Y
		length2 := dx*dx + dy*dy
		if length2 < 1e-12 {
			continue
		}
		t := math.Max(0, math.Min(1, ((center.X-a.X)*dx+(center.Y-a.Y)*dy)/length2))
		nearest := sitenav.Point{X: a.X + t*dx, Y: a.Y + t*dy}
		if nearest.Distance(*center) < personalSpaceRadius {
			return false
		}
	}
	return true
}
