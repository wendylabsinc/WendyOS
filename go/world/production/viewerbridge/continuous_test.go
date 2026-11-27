package viewerbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func TestContinuousSignedHTTPBeyondReadBudgetAndRecovery(t *testing.T) {
	f, api := setup(t)
	f.cfg.Continuous = true
	f.cfg.People.MaxTicks, f.cfg.Hazards.MaxTicks = 480, 480
	h := startEvidenceHTTP(t, f, api, nil)
	people, hazards := h.bridge.session.people, h.bridge.session.hazards
	var original client.Snapshot
	read := func() (int, client.Snapshot) {
		t.Helper()
		code, raw := h.request(t, http.MethodGet, "/v1/components", "read", nil)
		var got client.Snapshot
		if code == http.StatusOK && json.Unmarshal(raw, &got) != nil {
			t.Fatal("invalid component response")
		}
		return code, got
	}
	for i := 0; i < 485; i++ {
		if i == 10 {
			f.mu.Lock()
			f.mode = "unavailable"
			f.mu.Unlock()
			if code, got := read(); code != http.StatusServiceUnavailable || len(got.Components) != 0 {
				t.Fatal("temporary source failure was not unavailable", code)
			}
			f.mu.Lock()
			f.mode = ""
			f.mu.Unlock()
		}
		code, got := read()
		if code != http.StatusOK || len(got.Components) != 3 {
			t.Fatalf("completed read %d: status %d, count %d", i+1, code, len(got.Components))
		}
		if i == 0 {
			original = got
		} else if !reflect.DeepEqual(got, original) {
			t.Fatal("continuous read changed original components or cursor")
		}
		if people != h.bridge.session.people || hazards != h.bridge.session.hazards {
			t.Fatal("continuous read recreated source sessions")
		}
	}
	// The source clock is controlled by the test server, not HTTP request count.
	// Advancing it beyond the originals still produces a verified empty read.
	f.mu.Lock()
	f.now = f.now.Add(5 * time.Second)
	f.mu.Unlock()
	if code, got := read(); code != http.StatusOK || len(got.Components) != 0 {
		t.Fatal("continuous mode renewed original sample validity", code)
	}
	if err := h.bridge.session.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	queries := f.queries
	f.mu.Unlock()
	if code, _ := read(); code != http.StatusServiceUnavailable {
		t.Fatal("closed session remained readable", code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queries != queries {
		t.Fatal("closed session called native source")
	}
}

func TestContinuousPhysicalExpiryAndIdentityFence(t *testing.T) {
	f, api := setup(t)
	f.cfg.Continuous = true
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Query(context.Background()); err != nil || len(got) != 3 {
		t.Fatal("initial read failed", err)
	}
	f.mu.Lock()
	f.w.Earliest = f.w.Earliest.Add(6 * time.Second)
	f.w.Latest = f.w.Latest.Add(6 * time.Second)
	f.w.Monotonic += 6 * time.Second
	f.mu.Unlock()
	if got, err := s.Query(context.Background()); err != nil || len(got) != 0 {
		t.Fatal("continuous mode renewed original physical validity", err)
	}
	f.mu.Lock()
	f.mode = "wrong-generation"
	f.mu.Unlock()
	if got, err := s.Query(context.Background()); err == nil || got != nil {
		t.Fatal("changed mounted identity accepted")
	}
	f.mu.Lock()
	f.mode = ""
	f.mu.Unlock()
	if got, err := s.Query(context.Background()); err == nil || got != nil {
		t.Fatal("continuous session reset its identity fence")
	}
}

func TestBoundedDefaultStillStopsAtReadBudget(t *testing.T) {
	f, api := setup(t)
	f.cfg.People.MaxTicks, f.cfg.Hazards.MaxTicks = 1, 1
	raw, err := json.Marshal(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["continuous"] != nil {
		t.Fatal("bounded default changed config wire")
	}
	h := startEvidenceHTTP(t, f, api, nil)
	for i, want := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		if code, _ := h.request(t, http.MethodGet, "/v1/components", "read", nil); code != want {
			t.Fatalf("bounded read %d: %d, want %d", i+1, code, want)
		}
	}
}

func TestContinuousStartupDenialRecoversWithoutResettingSession(t *testing.T) {
	f, api := setup(t)
	f.cfg.Continuous = true
	h := startEvidenceHTTP(t, f, api, nil)
	people, hazards := h.bridge.session.people, h.bridge.session.hazards
	for i, step := range []struct {
		mode string
		code int
	}{
		{"denied", http.StatusServiceUnavailable}, // Source has not published yet.
		{"", http.StatusOK},
		{"unavailable", http.StatusServiceUnavailable},
		{"", http.StatusOK},
		{"denied", http.StatusServiceUnavailable}, // Denial after readiness stays terminal.
		{"", http.StatusServiceUnavailable},
	} {
		f.mu.Lock()
		f.mode = step.mode
		f.mu.Unlock()
		code, raw := h.request(t, http.MethodGet, "/v1/components", "read", nil)
		if code != step.code {
			t.Fatalf("startup/readiness transition %d: status %d, want %d", i, code, step.code)
		}
		if code == http.StatusOK {
			var got client.Snapshot
			if json.Unmarshal(raw, &got) != nil || len(got.Components) != 3 {
				t.Fatal("recovered read did not return the original components")
			}
		}
		if people != h.bridge.session.people || hazards != h.bridge.session.hazards {
			t.Fatal("startup recovery recreated pinned readers")
		}
	}
}
