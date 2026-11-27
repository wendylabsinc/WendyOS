//go:build race

package history

import "time"

// Shorter runs under the race detector: see race_off_test.go. The window
// still holds every scheduled change (250 to 285 s), the restart and the
// compaction horizon.
const (
	acceptFrom   = 247 * time.Second
	propertyRuns = 2
)
