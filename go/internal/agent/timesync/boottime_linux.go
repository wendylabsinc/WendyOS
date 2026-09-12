//go:build linux

package timesync

import "golang.org/x/sys/unix"

func bootTimeNanos() (int64, error) {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return ts.Nano(), err
}
