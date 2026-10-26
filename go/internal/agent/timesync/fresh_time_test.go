package timesync

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

func freshConsensus(t *testing.T) Consensus {
	t.Helper()
	boot, err := bootTimeNanos()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return Consensus{Confidence: ConfidenceDegraded, Quorum: 2,
		LowerOffsetNanos: now.Add(-time.Second).UnixNano() - boot,
		UpperOffsetNanos: now.Add(time.Second).UnixNano() - boot}
}

func TestFreshTimeAlwaysQueriesWithoutClockPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clock_floor")
	original := []byte("malformed boot hint must not block authenticated reset time")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewManager(nil, dir)
	m.query = func(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > maxCaptureRTT || len(servers) != len(Servers) {
			t.Fatal("fresh query must use bounded deadline and baked-in servers")
		}
		return freshConsensus(t), nil
	}
	m.apply = func(time.Time) { t.Fatal("reset time must not discipline the clock") }
	m.RecordConsensus(Consensus{Confidence: ConfidenceVerified, Quorum: 3})
	for range 2 {
		window, err := m.FreshTime(context.Background())
		if err != nil || window.Latest.Before(window.Earliest) || window.Earliest.Before(time.Now().Add(-2*time.Second)) {
			t.Fatalf("fresh window = %+v, %v", window, err)
		}
	}
	if calls != 2 {
		t.Fatalf("queries = %d; cached consensus is not reset authority", calls)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("fresh query changed clock floor: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("fresh query created persistent files: %v", err)
	}
}

func TestFreshTimeRejectsUnavailableOrInvalidConsensus(t *testing.T) {
	for _, name := range []string{"offline", "canceled", "weak", "single", "reversed", "overflow", "implausible"} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.query = func(context.Context, []roughtime.Server) (Consensus, error) {
				c := freshConsensus(t)
				switch name {
				case "offline":
					return Consensus{}, errors.New("offline")
				case "canceled":
					cancel()
				case "weak":
					c.Confidence = ConfidenceUnbounded
				case "single":
					c.Quorum = 1
				case "reversed":
					c.UpperOffsetNanos = c.LowerOffsetNanos - 1
				case "overflow":
					c.UpperOffsetNanos = math.MaxInt64
				case "implausible":
					c.LowerOffsetNanos, c.UpperOffsetNanos = 0, 1
				}
				return c, nil
			}
			if _, err := m.FreshTime(ctx); err == nil {
				t.Fatal("invalid fresh time must fail closed")
			}
		})
	}
}

func TestFreshTimeKeepsQuorumWhenOtherAuthoritiesTimeOut(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	caller := context.Background()
	m.query = func(collection context.Context, _ []roughtime.Server) (Consensus, error) {
		// QueryConsensus waits for every authority. Two valid responses can
		// already establish a quorum while the remaining requests time out.
		consensus := freshConsensus(t)
		<-collection.Done()
		if !errors.Is(collection.Err(), context.DeadlineExceeded) || caller.Err() != nil {
			t.Fatal("expected only the internal collection deadline to expire")
		}
		return consensus, nil
	}
	window, err := m.FreshTime(caller)
	if err != nil || window.Latest.Before(window.Earliest) || window.Earliest.Before(time.Now().Add(-2*time.Second)) {
		t.Fatalf("valid quorum was discarded after another authority timed out: %+v, %v", window, err)
	}
}

func TestFreshTimeStillRejectsExpiredCallerWithQuorum(t *testing.T) {
	m := NewManager(nil, t.TempDir())
	caller, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	m.query = func(collection context.Context, _ []roughtime.Server) (Consensus, error) {
		consensus := freshConsensus(t)
		<-collection.Done()
		return consensus, nil
	}
	if _, err := m.FreshTime(caller); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired caller was accepted despite a quorum: %v", err)
	}
}
