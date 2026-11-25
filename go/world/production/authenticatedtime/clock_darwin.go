//go:build darwin

package authenticatedtime

import (
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
	"time"
)

// Darwin clock_gettime(3) defines MONOTONIC_RAW as sleep-inclusive and free of
// frequency/time adjustment; UPTIME_RAW (and mach_absolute_time) omit sleep.
func readNative() (trustedtime.Reading, error) {
	raw, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	var ts unix.Timespec
	if err = unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts); err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	return trustedtime.Reading{BootID: id.String(), Elapsed: time.Duration(ts.Nano())}, nil
}
