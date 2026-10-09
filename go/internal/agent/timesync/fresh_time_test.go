package timesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

func freshTimeQuery(t *testing.T, lower time.Time) func(context.Context, []roughtime.Server) (Consensus, error) {
	t.Helper()
	return func(ctx context.Context, _ []roughtime.Server) (Consensus, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("fresh query has no deadline")
		}
		boot, err := bootTimeNanos()
		return floorConsensus(lower, boot), err
	}
}

func TestFreshTimeQueriesAndPersistsWithoutApplyingWallClock(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	query := freshTimeQuery(t, base)
	queries := 0
	m.query = func(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
		queries++
		return query(ctx, servers)
	}
	m.apply = func(time.Time) { t.Fatal("FreshTime changed wall clock") }
	window, err := m.FreshTime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if queries != 1 || window.Earliest.Before(base) || window.Latest.Sub(window.Earliest) != 2*time.Second {
		t.Fatalf("queries = %d, window = %+v", queries, window)
	}
	floor, _ := readFloor(m.configPath)
	if floor.Unix() != window.Earliest.Unix() {
		t.Fatalf("durable floor %v != lower bound %v", floor, window.Earliest)
	}
	if _, err := m.FreshTime(context.Background()); err != nil || queries != 2 {
		t.Fatalf("subsequent call reused cache or failed: queries = %d, err = %v", queries, err)
	}
}

func TestFreshTimeNeverFallsBackToCachedConsensus(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	m.RecordConsensus(floorConsensus(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), 0))
	m.query = func(context.Context, []roughtime.Server) (Consensus, error) {
		return Consensus{}, errors.New("authenticated source unavailable")
	}
	if _, err := m.FreshTime(context.Background()); err == nil {
		t.Fatal("cached confidence authorized time despite failed fresh query")
	}
	if _, err := os.Stat(filepath.Join(m.configPath, clockFloorFile)); !os.IsNotExist(err) {
		t.Fatalf("failed query wrote a floor: %v", err)
	}
}

func TestFreshTimeRejectsCanceledQueryBeforeWritingState(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query := freshTimeQuery(t, time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	m.query = func(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
		c, err := query(ctx, servers)
		cancel()
		return c, err
	}
	if _, err := m.FreshTime(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query = %v", err)
	}
	if entries, err := os.ReadDir(m.configPath); err != nil || len(entries) != 0 {
		t.Fatalf("canceled query wrote state: %v, %v", entries, err)
	}
}

func TestFreshTimeRejectsWeakOrInvalidIntervals(t *testing.T) {
	for _, name := range []string{"weak", "inverted", "implausible-upper", "missing-floor-directory"} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, t.TempDir())
			query := freshTimeQuery(t, time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
			m.query = func(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
				c, err := query(ctx, servers)
				switch name {
				case "weak":
					c.Confidence, c.Quorum = ConfidenceUnbounded, 1
				case "inverted":
					c.UpperOffsetNanos = c.LowerOffsetNanos - 1
				case "implausible-upper":
					c.UpperOffsetNanos += int64(100 * 365 * 24 * time.Hour)
				}
				return c, err
			}
			if name == "missing-floor-directory" {
				m.configPath = filepath.Join(m.configPath, "missing")
			}
			if _, err := m.FreshTime(context.Background()); err == nil {
				t.Fatal("accepted invalid or non-durable time")
			}
		})
	}
}

func TestFreshTimeRespectsExistingAntiRollbackFloor(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	future := base.Add(time.Hour)
	if err := WriteFloor(m.configPath, future); err != nil {
		t.Fatal(err)
	}
	m.query = freshTimeQuery(t, base)
	if _, err := m.FreshTime(context.Background()); err == nil {
		t.Fatal("fresh query bypassed newer anti-rollback floor")
	}
	floor, _ := readFloor(m.configPath)
	if !floor.Equal(future) {
		t.Fatalf("newer floor changed: %v", floor)
	}
}
