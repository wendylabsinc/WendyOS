package livecapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Captures have millisecond wall-clock resolution, while sequence and simulation
// nanoseconds identify distinct physics states. Wall time must not be rounded up.
func TestCaptureAcceptsAdvancedPhysicsWithinSameWallMillisecond(t *testing.T) {
	now := time.Unix(1800000000, 0)
	snapshot := fixture(now)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(snapshot)
	}))
	defer server.Close()
	cfg := testConfig(&now)
	cfg.URL = server.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	advanceCapture(&snapshot, 10*time.Millisecond)
	mu.Unlock()
	got, err := c.Capture(context.Background())
	if err != nil {
		t.Fatalf("distinct physics capture in same wall millisecond rejected: %v", err)
	}
	if got.Sequence != 2 || got.SampleUnixMS != now.UnixMilli() {
		t.Fatalf("capture identity/time was rewritten: sequence=%d wall=%d", got.Sequence, got.SampleUnixMS)
	}
	// Accepting equal wall milliseconds must not grant new freshness.
	now = now.Add(501 * time.Millisecond)
	if _, err = c.Capture(context.Background()); !errors.Is(err, ErrStale) {
		t.Fatalf("duplicate renewed freshness: %v", err)
	}
	mu.Lock()
	advanceCapture(&snapshot, 10*time.Millisecond)
	mu.Unlock()
	if _, err = c.Capture(context.Background()); !errors.Is(err, ErrStale) {
		t.Fatalf("higher sequence renewed stale wall capture: %v", err)
	}
}

func advanceCapture(s *Snapshot, delta time.Duration) {
	s.Sequence++
	s.SimulationTimeNS += int64(delta)
	s.State.SimulationTimeS = float64(s.SimulationTimeNS) / 1e9
	for i := range s.Observations {
		x := &s.Observations[i]
		x.Sequence = s.Sequence
		x.SampleNS = s.SimulationTimeNS
		x.ID = fmt.Sprintf("%s/%s/%d", x.RunID, x.SensorID, x.Sequence)
	}
}

func TestCaptureStillRejectsClockRollbackAndUnadvancedPhysics(t *testing.T) {
	for _, mode := range []string{"wall rollback", "simulation rollback", "unchanged physics"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			snapshot := fixture(now)
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				_ = json.NewEncoder(w).Encode(snapshot)
			}))
			defer server.Close()
			cfg := testConfig(&now)
			cfg.URL = server.URL
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Capture(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			switch mode {
			case "wall rollback":
				advanceCapture(&snapshot, time.Millisecond)
				snapshot.SampleUnixMS--
				snapshot.State.SampleUnixMS = snapshot.SampleUnixMS
			case "simulation rollback":
				advanceCapture(&snapshot, -time.Millisecond)
			case "unchanged physics":
				advanceCapture(&snapshot, 0)
			}
			mu.Unlock()
			if _, err = c.Capture(context.Background()); err == nil {
				t.Fatal("accepted clock failure")
			}
			mu.Lock()
			snapshot = fixture(now)
			mu.Unlock()
			if _, err = c.Capture(context.Background()); err == nil {
				t.Fatal("clock failure did not latch")
			}
		})
	}
}
