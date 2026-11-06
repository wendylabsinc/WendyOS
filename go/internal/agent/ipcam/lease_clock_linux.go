//go:build linux

package ipcam

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func leaseBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("empty kernel boot ID")
	}
	return id, nil
}

// CLOCK_BOOTTIME includes suspend and is unaffected by wall-clock corrections.
// Unlike Go's process-local monotonic timestamp, it survives agent restarts.
func leaseBootTime() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}
	return time.Duration(ts.Nano()), nil
}
