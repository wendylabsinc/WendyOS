//go:build linux

package appclock

import (
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
)

func readNative() (trustedtime.Reading, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	id, err := uuid.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts) != nil {
		return trustedtime.Reading{}, ErrUnavailable
	}
	return trustedtime.Reading{BootID: id.String(), Elapsed: time.Duration(ts.Nano())}, nil
}
