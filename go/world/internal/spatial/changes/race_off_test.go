//go:build !race

package changes

import "time"

// The full run: four epochs, so that the climber has been through every
// upstairs room after the last scheduled change.
const (
	officeRunFor    = 480 * time.Second
	quietRunFor     = 480 * time.Second
	officeMinScored = 8
)

var officeStarts = []time.Duration{0, 45 * time.Second}
