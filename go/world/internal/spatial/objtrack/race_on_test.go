//go:build race

package objtrack

import "time"

// mapFrom is where the static map of the acceptance test begins. Building
// the reference map is the expensive part under the race detector (about
// twelve times slower), so there the map covers the last 100 s before the
// tracking window, which is still longer than every route's loop but the
// climber's. The tracking window and every threshold are the same.
const mapFrom = 125 * time.Second
