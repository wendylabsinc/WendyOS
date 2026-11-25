//go:build linux

package timesync

import (
	"fmt"
	"os"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// AdvanceTo advances the system clock to t if t is after the current time.
// It performs a step correction via settimeofday then persists to the hardware
// RTC so the time survives a power cycle.
func AdvanceTo(t time.Time, logger *zap.Logger) error {
	now := time.Now()
	if !t.After(now) {
		return nil // never go backward
	}
	offset := t.Sub(now)

	tv := unix.Timeval{
		Sec:  t.Unix(),
		Usec: int64(t.Nanosecond()) / 1000,
	}
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}

	if err := setRTC(t); err != nil && logger != nil {
		asyncdiag.Log(logger, zap.WarnLevel, "timesync: could not set hardware RTC", zap.Error(err))
	}
	if logger != nil {
		asyncdiag.Log(logger, zap.InfoLevel, "timesync: clock advanced",
			zap.Time("from", now),
			zap.Time("to", t),
			zap.Duration("offset", offset))
	}
	return nil
}

// RTC_SET_TIME ioctl number on Linux arm64/amd64.
const ioctlRTCSetTime = 0x4024700a

type rtcTime struct {
	Sec   int32
	Min   int32
	Hour  int32
	Mday  int32
	Mon   int32
	Year  int32
	Wday  int32
	Yday  int32
	Isdst int32
}

func setRTC(t time.Time) (retErr error) {
	utc := t.UTC()
	rt := rtcTime{
		Sec:  int32(utc.Second()),
		Min:  int32(utc.Minute()),
		Hour: int32(utc.Hour()),
		Mday: int32(utc.Day()),
		Mon:  int32(utc.Month() - 1), // rtc months: 0-11
		Year: int32(utc.Year() - 1900),
	}
	f, err := os.OpenFile("/dev/rtc0", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/rtc0: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close /dev/rtc0: %w", cerr)
		}
	}()
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), ioctlRTCSetTime, uintptr(unsafe.Pointer(&rt)))
	if errno != 0 {
		return fmt.Errorf("RTC_SET_TIME ioctl: %w", errno)
	}
	return nil
}
