package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSO101ObserverSceneAndCreate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || (r.URL.Path != "/api/scene" && r.URL.Path != "/api/scene/state") {
			t.Errorf("unexpected observer request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/scene" {
			_, _ = w.Write([]byte(`{"version":1,"id":"arm","bodies":[],"geoms":[],"meshes":[]}`))
		} else {
			_, _ = w.Write([]byte(`{"scene_id":"arm","epoch":1,"generation":1,"time":0,"valid":true,"mode":"joint-control","positions":[],"quaternions":[]}`))
		}
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "so101")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "so101", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/scene", "/api/scene/state"} {
		req, _ := http.NewRequest("GET", session["url"].(string)+path, nil)
		req.Header.Set("Authorization", "Bearer "+session["token"].(string))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s status %d", path, res.StatusCode)
		}
	}
	s := New(nil, nil)
	s.SetSimulatorBackend(SimulatorBackend{Create: func(_ context.Context, o SimulatorCreateOptions) (*SimulatorInfo, error) {
		if o.Profile != "so101" {
			t.Fatalf("profile %s", o.Profile)
		}
		return &SimulatorInfo{Name: o.Name, Profile: o.Profile, State: "stopped", Device: "vm:" + o.Name}, nil
	}})
	result, err := s.handleSimulatorCreate(context.Background(), callToolReq("simulator_create", map[string]any{"name": "arm", "profile": "so101"}))
	if err != nil || result.IsError {
		t.Fatalf("SO101 create %v %v", result, err)
	}
}
