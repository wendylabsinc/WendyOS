//go:build !race

package cslam

import "time"

// officeWindow is the simulated time the Office acceptance test runs for:
// long enough for every rover to complete its route once and revisit its
// start, so that odometry drift, loop closures and inter-robot closures all
// take part.
const officeWindow = 120 * time.Second
