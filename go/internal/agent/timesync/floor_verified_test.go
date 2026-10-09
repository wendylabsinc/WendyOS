package timesync

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

func floorConsensus(lower time.Time, boot int64) Consensus {
	return Consensus{Confidence: ConfidenceVerified, Quorum: 3,
		LowerOffsetNanos: lower.UnixNano() - boot,
		UpperOffsetNanos: lower.Add(2*time.Second).UnixNano() - boot}
}

func TestVerifiedFloorUsesLowerBoundAndNeverRegresses(t *testing.T) {
	path := t.TempDir()
	m := NewManager(nil, path)
	boot := int64(10 * time.Second)
	lower := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := m.persistConsensusFloor(floorConsensus(lower, boot), boot); err != nil {
		t.Fatal(err)
	}
	if floor, refused := readFloor(path); !floor.Equal(lower) || !refused.IsZero() {
		t.Fatalf("floor = %v, refused = %v; want authenticated lower bound %v", floor, refused, lower)
	}
	if err := m.persistConsensusFloor(floorConsensus(lower.Add(-time.Hour), boot), boot); err != nil {
		t.Fatal(err)
	}
	if floor, _ := readFloor(path); !floor.Equal(lower) {
		t.Fatalf("floor moved backward: %v", floor)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != clockFloorFile {
		t.Fatalf("unexpected persistent files: %v, %v", entries, err)
	}
}

func TestVerifiedFloorSerializesConcurrentUpdates(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.persistConsensusFloor(floorConsensus(base.Add(time.Duration(i)*time.Second), 0), 0)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if floor, _ := readFloor(m.configPath); !floor.Equal(base.Add(31 * time.Second)) {
		t.Fatalf("did not preserve newest concurrent floor: %v", floor)
	}
}

func TestVerifiedFloorRejectsInvalidConsensus(t *testing.T) {
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"weak", "quorum", "interval", "overflow", "epoch", "negative-boot"} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, t.TempDir())
			c, boot := floorConsensus(base, 0), int64(0)
			switch name {
			case "weak":
				c.Confidence = ConfidenceUnbounded
			case "quorum":
				c.Quorum = 1
			case "interval":
				c.UpperOffsetNanos = c.LowerOffsetNanos - 1
			case "overflow":
				c.LowerOffsetNanos, c.UpperOffsetNanos, boot = math.MaxInt64, math.MaxInt64, 1
			case "epoch":
				c = floorConsensus(time.Unix(0, 0), 0)
			case "negative-boot":
				boot = -1
			}
			if err := m.persistConsensusFloor(c, boot); err == nil {
				t.Fatal("accepted invalid consensus")
			}
			entries, err := os.ReadDir(m.configPath)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid evidence wrote state: %v, %v", entries, err)
			}
		})
	}
}

func TestVerifiedFloorRetainsMalformedOrLinkedState(t *testing.T) {
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"short", "implausible", "symlink"} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, t.TempDir())
			path := filepath.Join(m.configPath, clockFloorFile)
			original := []byte{1, 2, 3}
			if name == "implausible" {
				original = FloorBytes(time.Unix(0, 0))
			}
			target := path
			if name == "symlink" {
				target = filepath.Join(t.TempDir(), "target")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := m.persistConsensusFloor(floorConsensus(base, 0), 0); err == nil {
				t.Fatal("silently replaced invalid existing state")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != string(original) {
				t.Fatalf("existing state changed: %v, %v", data, err)
			}
		})
	}
}

func TestRunDirectPersistsAuthenticatedFloorBeforeApplyingClock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := t.TempDir()
	m := NewManager(nil, path)
	m.query = func(context.Context, []roughtime.Server) (Consensus, error) {
		boot, err := bootTimeNanos()
		c := floorConsensus(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), boot)
		c.Confidence, c.Quorum = ConfidenceDegraded, 2
		return c, err
	}
	applied := false
	m.apply = func(midpoint time.Time) {
		applied = true
		floor, refused := readFloor(path)
		if !refused.IsZero() || floor.Unix() != midpoint.Add(-time.Second).Unix() {
			t.Fatalf("floor = %v, refused = %v, applied midpoint = %v", floor, refused, midpoint)
		}
		cancel()
	}
	m.sleep = func(time.Duration) <-chan time.Time { return nil }
	m.RunDirect(ctx)
	if !applied {
		t.Fatal("verified consensus did not apply")
	}
	// A new manager sees exactly the persisted lower bound without retaining
	// any in-memory consensus or relying on an additional recovery file.
	floor, refused := readFloor(NewManager(nil, path).configPath)
	if floor.IsZero() || !refused.IsZero() {
		t.Fatalf("restart lost authenticated floor: %v, %v", floor, refused)
	}
}

func TestRunDirectRetriesFloorWriteFailureWithoutApplyingClock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(nil, filepath.Join(t.TempDir(), "missing"))
	m.query = func(context.Context, []roughtime.Server) (Consensus, error) {
		boot, err := bootTimeNanos()
		return floorConsensus(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), boot), err
	}
	m.apply = func(time.Time) { t.Fatal("applied clock despite failed floor persistence") }
	m.sleep = func(d time.Duration) <-chan time.Time {
		if d != backoffSchedule[0] {
			t.Fatalf("wait = %v, want retry backoff", d)
		}
		cancel()
		return nil
	}
	m.RunDirect(ctx)
}
