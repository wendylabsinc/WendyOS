package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestTourReviewSignsSelectedOrderAndFixedDestinations(t *testing.T) {
	rooms := []patrol.Room{{ID: "office"}, {ID: "lounge"}, {ID: "unselected"}}
	path := filepath.Join(t.TempDir(), "tour.json")
	raw := `{"tour":{"method":"inspect-selected-rooms-once-and-return-v1","stops":[{"room_id":"lounge","position":[1,0.3,1],"yaw":0.2},{"room_id":"office","position":[2,0.3,1],"yaw":0.4}]},"return":{"position":[0,0.3,0],"yaw":0,"before_expiry_seconds":0}}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readPatrolConfig(path, rooms)
	if err != nil {
		t.Fatal(err)
	}
	store, _, _, spec, err := fixture(t.TempDir(), "finite-tour-test", world.FrameRef{ID: "studio", Revision: "v1"}, rooms, 600, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var signed authority.SignedInstruction
	if err = json.Unmarshal(record.Proof, &signed); err != nil {
		t.Fatal(err)
	}
	var payload tasking.Spec
	if err = json.Unmarshal(signed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Patrol.Rooms) != 2 || payload.Patrol.Rooms[0] != "lounge" || payload.Patrol.Rooms[1] != "office" || payload.Patrol.Tour.Stops[1].Position[0] != 2 || payload.Patrol.Tour.Stops[1].Yaw != .4 || payload.Patrol.Return == nil || len(payload.Patrol.RevisitSeconds) != 0 {
		t.Fatal("signed payload changed selected tour", payload.Patrol)
	}
	cfg.Tour.Stops[0].RoomID = "not-commissioned"
	if _, err = tourRooms(rooms, cfg.Tour); err == nil {
		t.Fatal("unknown room accepted")
	}
}

func TestTourGoalSelectorAndLegacyCapability(t *testing.T) {
	mode := ""
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/status" {
			json.NewEncoder(w).Encode(map[string]any{"inspection_mode": mode})
			return
		}
		if r.URL.Path != "/goal" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
	}))
	defer server.Close()
	a := &adapter{nav: server.URL, client: server.Client()}
	if a.requireInspectionNavigation() == nil {
		t.Fatal("old adapter falsely supports signed yaw")
	}
	mu.Lock()
	mode = patroltour.NavigationMode
	mu.Unlock()
	if err := a.requireInspectionNavigation(); err != nil {
		t.Fatal(err)
	}
	for _, inspection := range []bool{true, false} {
		a.inspectionGoal = func() bool { return inspection }
		if err := a.Goal("goal", patrol.Point{X: 1, Y: 2, Yaw: .4}); err != nil {
			t.Fatal(err)
		}
	}
	a.inspectionGoal = nil
	if err := a.Goal("ordinary", patrol.Point{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies[0]["observation_mode"] != patroltour.NavigationMode {
		t.Fatal("tour omitted strict mode")
	}
	for _, body := range bodies[1:] {
		if _, ok := body["observation_mode"]; ok {
			t.Fatal("return/ordinary goal changed", body)
		}
	}
}
