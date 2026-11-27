//go:build linux

package authenticatedtime

import (
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"time"
)

func readNative() (trustedtime.Reading, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	id, err := uuid.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	var ts unix.Timespec
	if err = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return trustedtime.Reading{}, nativeError(err)
	}
	return trustedtime.Reading{BootID: id.String(), Elapsed: time.Duration(ts.Nano())}, nil
}
