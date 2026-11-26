//go:build !race

package anchors

import "time"

// mapWindow is the simulated time the mapping robot drives before the anchors
// are placed, one lap of its route. lateLooks are the times after its start
// at which the late device looks for the anchors: three places on its lap.
const mapWindow = 85 * time.Second

var lateLooks = []time.Duration{8 * time.Second, 14 * time.Second, 20 * time.Second}
