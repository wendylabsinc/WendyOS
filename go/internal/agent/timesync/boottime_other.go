//go:build !linux

package timesync

import "time"

var localProcessBoot = time.Now()

func bootTimeNanos() (int64, error) { return time.Since(localProcessBoot).Nanoseconds(), nil }
