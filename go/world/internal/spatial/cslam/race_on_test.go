//go:build race

package cslam

import "time"

// officeWindow is the simulated time the Office acceptance test runs for.
// Under the race detector it is the shortest window in which every robot is
// aligned and closes loops with the others; the full window runs without it.
const officeWindow = 40 * time.Second
