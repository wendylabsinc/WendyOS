package timesync

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const clockFloorFile = "clock_floor"

// A floor outside this window is treated as absent. It catches a torn or zeroed
// write, not a wrong-but-plausible one: the CLI writes this file from the
// flashing host's own clock (cli/commands/os_provision.go), so a host that is
// merely months or years out still produces a value inside the window. Since
// AdvanceTo never moves the clock backward, a future floor parks the device there
// until it is reflashed, and only the grossly-wrong cases are caught here.
var (
	floorMin = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	floorMax = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// readFloor reads the Unix-seconds timestamp from configPath/clock_floor. floor
// is the value to apply and is zero when there is nothing to apply — an absent or
// short file, the normal pre-feature case, or a value outside the plausible
// window. Such a value comes back as refused instead, which is non-zero only in
// that case, so it can be logged (an all-zero write, a 1970 host clock and a
// year-2200 value need different remedies) without any path applying it.
func readFloor(configPath string) (floor, refused time.Time) {
	data, err := os.ReadFile(filepath.Join(configPath, clockFloorFile))
	if err != nil || len(data) < 8 {
		return time.Time{}, time.Time{}
	}
	sec := int64(binary.BigEndian.Uint64(data[:8])) //nolint:gosec — range-checked below
	t := time.Unix(sec, 0)
	if t.Before(floorMin) || t.After(floorMax) {
		return time.Time{}, t
	}
	return t, time.Time{}
}

// advanceVerifiedFloor persists an authenticated lower bound. The caller must
// serialize updates and obtain t from verified evidence, never from time.Now.
// Unlike install-time WriteFloor, ordinary sync cannot replace a newer floor.
func advanceVerifiedFloor(configPath string, t time.Time) error {
	if configPath == "" || t.Before(floorMin) || t.After(floorMax) {
		return fmt.Errorf("timesync: invalid verified clock floor")
	}
	path := filepath.Join(configPath, clockFloorFile)
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("timesync: reading floor metadata: %w", err)
	}
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() != 8 {
			return fmt.Errorf("timesync: existing clock floor is not a regular timestamp file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("timesync: reading clock floor: %w", err)
		}
		if len(data) != 8 {
			return fmt.Errorf("timesync: incomplete clock floor")
		}
		old := time.Unix(int64(binary.BigEndian.Uint64(data)), 0) //nolint:gosec — range-checked below
		if old.Before(floorMin) || old.After(floorMax) {
			return fmt.Errorf("timesync: existing clock floor is implausible")
		}
		if t.Unix() <= old.Unix() {
			return nil
		}
	}
	f, err := os.CreateTemp(configPath, ".clock_floor-*")
	if err != nil {
		return fmt.Errorf("timesync: creating floor update: %w", err)
	}
	defer os.Remove(f.Name()) //nolint:errcheck — also harmless after rename
	defer f.Close()           //nolint:errcheck — explicitly closed before rename
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.Write(FloorBytes(t)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(configPath)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck — Sync reports durability errors
	return dir.Sync()
}

// FloorBytes encodes t as the 8-byte big-endian clock_floor payload.
func FloorBytes(t time.Time) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(t.Unix())) //nolint:gosec — install-time timestamps are non-negative
	return buf[:]
}

// WriteFloor writes t as a big-endian int64 Unix timestamp to
// configPath/clock_floor. Called by the CLI at install time and during
// config-partition provisioning.
func WriteFloor(configPath string, t time.Time) error {
	return os.WriteFile(filepath.Join(configPath, clockFloorFile), FloorBytes(t), 0o644)
}
