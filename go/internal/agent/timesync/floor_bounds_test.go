package timesync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// /config/clock_floor is eight bytes on a FAT partition that anyone flashing a
// card can write. Since AdvanceTo only moves the clock forward, a value in the
// future parks the device there and every certificate reads as expired until it
// is reflashed.
func TestReadFloor_RefusesImplausibleValues(t *testing.T) {
	cases := map[string][]byte{
		"all zero":    {0, 0, 0, 0, 0, 0, 0, 0},
		"all ones":    {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"far future":  FloorBytes(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)),
		"before 2024": FloorBytes(time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, clockFloorFile), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			floor, refused := readFloor(dir)
			if !floor.IsZero() {
				t.Errorf("floor = %v, want zero so no caller can apply it", floor)
			}
			if refused.IsZero() {
				t.Error("the refused value must be returned so it can be logged")
			}
		})
	}
}

// A short or absent file is the normal pre-feature case, not an anomaly, and must
// not be reported as a refused value.
func TestReadFloor_AbsentOrShortIsNotRefused(t *testing.T) {
	dir := t.TempDir()
	if floor, refused := readFloor(dir); !floor.IsZero() || !refused.IsZero() {
		t.Errorf("absent file: got %v / %v, want both zero", floor, refused)
	}
	if err := os.WriteFile(filepath.Join(dir, clockFloorFile), []byte{0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if floor, refused := readFloor(dir); !floor.IsZero() || !refused.IsZero() {
		t.Errorf("short file: got %v / %v, want both zero", floor, refused)
	}
}

func TestReadFloor_AcceptsPlausibleValue(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2026, 8, 7, 0, 33, 2, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, clockFloorFile), FloorBytes(want), 0o600); err != nil {
		t.Fatal(err)
	}
	floor, refused := readFloor(dir)
	if !floor.Equal(want) || !refused.IsZero() {
		t.Errorf("readFloor = %v / %v, want %v / zero", floor, refused, want)
	}
}

// An operator seeing only "ignoring implausible clock floor" cannot tell a zeroed
// write from a year-2200 value, which need different remedies.
func TestApplyFloor_LogsTheRefusedValue(t *testing.T) {
	dir := t.TempDir()
	bad := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, clockFloorFile), FloorBytes(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	NewManager(zap.New(core), dir).ApplyFloor()

	entries := logs.FilterMessageSnippet("implausible clock floor").All()
	if len(entries) != 1 {
		t.Fatalf("got %d warnings, want 1", len(entries))
	}
	got, ok := entries[0].ContextMap()["floor"].(time.Time)
	if !ok || !got.Equal(bad) {
		t.Errorf("logged floor = %v, want the refused value %v", got, bad)
	}
}

// ApplyFloor steps the clock through the manager's apply hook, so tests can
// check it without calling settimeofday, which as root on Linux really moves
// the host clock (WDY-3852).
func TestApplyFloor_AdvancesToAFutureFloor(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	if err := os.WriteFile(filepath.Join(dir, clockFloorFile), FloorBytes(future), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, dir)
	var applied []time.Time
	m.apply = func(t time.Time) { applied = append(applied, t) }

	m.ApplyFloor()

	if len(applied) != 1 || !applied[0].Equal(future) {
		t.Fatalf("applied %v, want the floor %v once", applied, future)
	}
}
