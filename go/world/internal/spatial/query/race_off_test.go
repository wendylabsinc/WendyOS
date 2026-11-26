//go:build !race

package query

import "time"

const (
	// raceEnabled gates wall-clock assertions: the race detector slows this
	// package's inner loops several times over and the machine is shared.
	raceEnabled = false
	// worldsPerSeed and queriesPerWorld size the property test.
	worldsPerSeed   = 150
	queriesPerWorld = 120
	// minHitsPerFeature is how many non-empty answers must exercise each
	// query feature, per seed, for the comparison to count.
	minHitsPerFeature = 40
	// simMapWindow is how much of the Office scenario the reference map of
	// the acceptance test is built from.
	simMapWindow = 240 * time.Second
	// simQueries is the number of random queries the acceptance test checks
	// against brute force on the Office world.
	simQueries = 1500
	// simPathQueries is how many of them keep a path clause.
	simPathQueries = 6
	// perfQueries is the number of queries timed per kind in the
	// performance test and perfRepeats the runs each is the best of.
	perfQueries = 24
	perfRepeats = 3
)
