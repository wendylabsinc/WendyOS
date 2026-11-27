//go:build darwin

package appclock

import (
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
)

func readNative() (trustedtime.Reading, error) {
	raw, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	// MONOTONIC_RAW includes sleep on Darwin; UPTIME_RAW does not.
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts) != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	return trustedtime.Reading{BootID: id.String(), Elapsed: time.Duration(ts.Nano())}, nil
}
