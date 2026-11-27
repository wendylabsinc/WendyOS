//go:build !race

package calib

import "time"

// The full run: the whole standard scenario.
const (
	officeRunFor = 600 * time.Second
	// officeMinValid is how many of the nine cameras must have reached a
	// valid calibration by the end. The others have a pose inside the
	// targets and say which uncertainty is still above its limit.
	officeMinValid = 5
	// Without the target and the landmark map: how many cameras must have a
	// pose, and how many a valid calibration, by the time of the knock.
	noTargetMinPosed = 7
	noTargetMinValid = 1
)
