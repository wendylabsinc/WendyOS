//go:build race

package changes

import "time"

// Under the race detector the run that starts mid-epoch is left out: it
// exercises no other code and doubles the cost.
const (
	officeRunFor    = 480 * time.Second
	quietRunFor     = 480 * time.Second
	officeMinScored = 8
)

var officeStarts = []time.Duration{0}
