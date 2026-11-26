//go:build race

package query

import "time"

// Under the race detector the same tests run on fewer worlds and a shorter
// map window, and no wall-clock assertion is made. The full sizes and the
// timing target are in race_off_test.go.
const (
	raceEnabled       = true
	worldsPerSeed     = 30
	queriesPerWorld   = 80
	minHitsPerFeature = 3
	simMapWindow      = 100 * time.Second // one full loop of rover-1: lobby, kitchen, office A
	simQueries        = 200
	simPathQueries    = 0 // the reference needs some 15 s per path over the Office map here
	perfQueries       = 3
	perfRepeats       = 1
)
