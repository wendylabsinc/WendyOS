//go:build darwin

package command

import (
	"golang.org/x/sys/unix"
	"time"
)

func annotationBootTime() time.Duration {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts) != nil {
		return -1
	}
	return time.Duration(ts.Nano())
}
