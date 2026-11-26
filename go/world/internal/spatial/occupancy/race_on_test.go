//go:build race

package occupancy

import "time"

// Ray casting is about twelve times slower under the race detector, so the
// fleet maps a shorter window there, long enough for the climber to cover
// most of the upper floor, and the property tests run fewer histories. The
// thresholds are the same.
const (
	acceptanceWindow = 60 * time.Second
	minObservedCells = 20000
	mergeSeeds       = 6
	syncSeeds        = 4
)
