//go:build race

package anchors

import "time"

// Under the race detector the late device looks for the anchors once instead
// of three times; the mapping lap cannot be shorter and still pass the lab.
const mapWindow = 85 * time.Second

var lateLooks = []time.Duration{8 * time.Second, 14 * time.Second, 20 * time.Second}
