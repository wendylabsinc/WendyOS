//go:build race

package calib

import "time"

// Under the race detector the simulator is about ten times slower, so the
// runs are cut to four minutes. That is before the camera knock, and fewer
// robot passes leave most cameras short of the validity gate; the accuracy
// targets are unchanged.
const (
	officeRunFor     = 240 * time.Second
	officeMinValid   = 1
	noTargetMinPosed = 6
	noTargetMinValid = 0
)
