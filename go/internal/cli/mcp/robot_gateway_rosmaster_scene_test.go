package mcp

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewayROSMasterObserverScene(t *testing.T) {
	paths := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("observer sent %s", r.Method)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/scene" {
			io.WriteString(w, `{"owner":"private-controller-token","geometry":{"wheelbase":0.26,"track":0.24,"wheel_radius":0.05,"length":0.4,"width":0.32,"private":"private-controller-token"},"lidar_offset":[0.13,0,0.28],"room_half_size":5,"obstacles":[]}`)
		} else if r.URL.Path == "/api/status" {
			io.WriteString(w, `{"epoch":3,"healthy":true,"mode":"driving","owner":"private-controller-token","control_mode":"browser","state":{"x":1,"y":2,"yaw":1.5707963267948966,"elapsed":12,"steering_angles":[0.1,0.2],"wheel_positions":[1,2,3,4]}}`)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "rosmaster-r2")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "rosmaster-r2", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	scene, _ := g.authorizedSimulatorScene(ctx, session["token"].(string))
	defer scene.cancel()
	body, err := g.readSimulatorScene(ctx, session["token"].(string), "geometry")
	if err != nil || !strings.Contains(string(body), `"profile":"rosmaster-r2"`) {
		t.Fatalf("geometry: %s %v", body, err)
	}
	if strings.Contains(string(body), "private-controller-token") || strings.Contains(string(body), "owner") {
		t.Fatal("geometry exposed controller authorization")
	}
	body, err = g.readSimulatorScene(ctx, session["token"].(string), "state")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private-controller-token") || strings.Contains(string(body), "owner") || strings.Contains(string(body), "control_mode") {
		t.Fatal("observer exposed controller authorization")
	}
	var state struct {
		Positions, Quaternions []float64
		SceneID                string `json:"scene_id"`
		Epoch                  int
		Time                   float64
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	if state.SceneID != rosmasterSceneID || state.Epoch != 3 || state.Time != 12 || len(state.Positions) != 18 || len(state.Quaternions) != 24 {
		t.Fatalf("state: %+v", state)
	}
	if math.Abs(state.Positions[6]-.88) > 1e-9 || math.Abs(state.Positions[7]-2.26) > 1e-9 || state.Positions[8] != .05 {
		t.Fatalf("front wheel world pose: %v", state.Positions[6:9])
	}
	for i := 0; i < len(state.Quaternions); i += 4 {
		norm := 0.
		for _, q := range state.Quaternions[i : i+4] {
			norm += q * q
		}
		if math.Abs(norm-1) > 1e-9 {
			t.Fatalf("invalid wheel quaternion %v", state.Quaternions[i:i+4])
		}
	}
	if strings.Join(paths, ",") != "/api/scene,/api/status" {
		t.Fatalf("observer paths: %v", paths)
	}
}

func TestSimulatorCreateROSMasterProfile(t *testing.T) {
	s := New(nil, nil)
	s.SetSimulatorBackend(SimulatorBackend{Create: func(_ context.Context, options SimulatorCreateOptions) (*SimulatorInfo, error) {
		if options.Profile != "rosmaster-r2" {
			t.Fatalf("profile: %s", options.Profile)
		}
		return &SimulatorInfo{Name: options.Name, Profile: options.Profile, State: "stopped", Device: "vm:" + options.Name}, nil
	}})
	result, err := s.handleSimulatorCreate(context.Background(), callToolReq("simulator_create", map[string]any{"name": "r2", "profile": "rosmaster-r2"}))
	if err != nil || result.IsError {
		t.Fatalf("ROSMaster create: %v %v", result, err)
	}
}
