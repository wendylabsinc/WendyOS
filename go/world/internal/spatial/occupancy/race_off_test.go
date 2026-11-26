//go:build !race

package occupancy

import "time"

const (
	// acceptanceWindow is how much of the Office scenario the fleet maps:
	// long enough for every route to close at least once (the longest loop
	// is the climber's 142 s) and short of the scheduled changes at 240 s.
	acceptanceWindow = 150 * time.Second
	// minObservedCells is the least a floor must contribute to the score.
	minObservedCells = 20000
	// mergeSeeds and syncSeeds are how many seeded histories the property
	// tests run.
	mergeSeeds = 20
	syncSeeds  = 10
)
