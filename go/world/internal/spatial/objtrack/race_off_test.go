//go:build !race

package objtrack

import "time"

// mapFrom is where the static map of the acceptance test begins: without
// the race detector, everything a mapper saw before the tracking window.
const mapFrom = 0 * time.Second
