//go:build !race

package history

import "time"

// The full-length runs. Under the race detector, where JSON and checksums
// cost ten times as much, race_on_test.go shortens them; every threshold is
// the same in both builds.
const (
	// acceptFrom is where the acceptance window starts in the Office scenario.
	acceptFrom = 235 * time.Second
	// propertyRuns is the number of random histories per seed.
	propertyRuns = 4
)
