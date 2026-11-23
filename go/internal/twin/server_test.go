package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRoomHTTPContributionAndRestart(t *testing.T) {
	directory := t.TempDir()
	store, _ := NewStore(directory)
	service := &Server{Store: store, Context: context.Background()}
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	r := room()
	r.Site.ID = strings.ToUpper(r.Site.ID)
	call := func(method, path string, value any, status int) []byte {
		t.Helper()
		b, _ := json.Marshal(value)
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(b))
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, out.String())
		}
		return out.Bytes()
	}
	path := "/v1/rooms/" + strings.ToLower(r.Site.ID)
	call("PUT", path, r, 200)
	call("POST", path+"/contributions", Contribution{Source: "apple:scan", Sequence: 1, ModelRevision: r.Site.ModelRevision, Transform: []float64{10, 1, 0, 0}, Points: []Point{{1, 2, 3}}}, 200)
	call("PUT", path, r, 409)
	var saved Room
	json.Unmarshal(call("GET", path, nil, 200), &saved)
	if len(saved.Points) != 1 || saved.Points[0] != (Point{11, 3, 3}) {
		t.Fatal(saved.Points)
	}
	restart, _ := NewStore(directory)
	again, e := restart.Get(r.Site.ID)
	if e != nil || again.Revision != saved.Revision {
		t.Fatalf("restart: %+v %v", again, e)
	}
	call("POST", path+"/contributions", map[string]any{"source": "bad", "unexpected": 1}, 400)
}
func TestPeopleExpireAndNeverRestoreOnRestart(t *testing.T) {
	now := time.Now()
	p := People{}
	at := float64(now.UnixNano()) / 1e9
	tracks, e := p.Update("room", []PersonObservation{{Camera: uuid.NewString(), Position: Point{1, 0, 2}, ObservedAt: at}}, now)
	if e != nil || len(tracks) != 1 {
		t.Fatalf("%v %v", tracks, e)
	}
	tracks, _ = p.Update("room", nil, now.Add(2*time.Second))
	if len(tracks) != 0 {
		t.Fatal("stale person visible")
	}
	restarted := People{}
	tracks, _ = restarted.Update("room", nil, now)
	if len(tracks) != 0 {
		t.Fatal("restored live reading")
	}
}
func TestFloorTilesRequireObservedNeighbors(t *testing.T) {
	if len(FloorTiles([]Point{{0, 0, 0}})) != 0 {
		t.Fatal("isolated point became floor")
	}
	points := []Point{{.05, 0, .05}, {-.05, 0, .05}, {.15, 0, .05}, {.05, 0, -.05}, {.05, 0, .15}}
	if len(FloorTiles(points)) != 6 {
		t.Fatal("supported floor missing")
	}
	points[4].Y = .5
	if len(FloorTiles(points)) != 0 {
		t.Fatal("step became level floor")
	}
}

func TestPeopleMergeAcrossNodesButKeepSeparatePeopleOnOneCamera(t *testing.T) {
	now := time.Now()
	at := float64(now.UnixNano()) / 1e9
	p := People{}
	a, b := uuid.NewString(), uuid.NewString()
	p.Update("room", []PersonObservation{{Camera: a, Position: Point{1, 0, 1}, ObservedAt: at}}, now)
	tracks, e := p.Update("room", []PersonObservation{{Camera: b, Position: Point{1.1, 0, 1}, ObservedAt: at}}, now)
	if e != nil || len(tracks) != 1 {
		t.Fatalf("duplicate multi-camera person: %+v %v", tracks, e)
	}
	tracks, e = p.Update("another", []PersonObservation{{Camera: a, Position: Point{1, 0, 1}, ObservedAt: at}, {Camera: a, Position: Point{1.1, 0, 1}, ObservedAt: at}}, now)
	if e != nil || len(tracks) != 2 {
		t.Fatalf("merged separate people on one camera: %+v %v", tracks, e)
	}
}
