package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSimulatorPoseHistoryRetainsFootLiftBetweenToolReads(t *testing.T) {
	var calls, active, maxActive atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/scene/state" {
			t.Error("collector accessed a control endpoint")
		}
		n := calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		if current > maxActive.Load() {
			maxActive.Store(current)
		}
		z := 0.
		if n == 4 {
			z = .18
		}
		fmt.Fprintf(w, `{"scene_id":"fixture","epoch":1,"generation":1,"time":%f,"positions":[0,0,%f],"quaternions":[0,0,0,1]}`, float64(n)/30, z)
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "g1")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "g1", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id := session["token"].(string)
	scene, _ := g.authorizedSimulatorScene(ctx, id)
	defer scene.cancel()
	read := func(after uint64) struct {
		Frames   []simulatorPoseFrame
		Sequence uint64
		Dropped  bool
	} { t.Helper(); result, err := g.protocol.ListTools()["simulator_scene_read"].Handler(ctx, callToolReq("simulator_scene_read", map[string]any{"session_id": id, "part": "state", "history": true, "after_sequence": after})); if err != nil || result.IsError || len(result.Content) != 0 || result.StructuredContent != nil {
		t.Fatal("history must be UI-only", result, err)
	}; var packet struct {
		Frames   []simulatorPoseFrame
		Sequence uint64
		Dropped  bool
	}; if json.Unmarshal(result.Meta.AdditionalFields["scene_data"].(json.RawMessage), &packet) != nil {
		t.Fatal("invalid packet")
	}; return packet }
	first := read(0)
	deadline := time.After(2 * time.Second)
	for {
		scene.history.mu.Lock()
		sequence, changed := scene.history.sequence, scene.history.changed
		scene.history.mu.Unlock()
		if sequence >= 9 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("intermediate poses were not captured")
		}
	}
	second := read(first.Sequence)
	foundLift, foundLanding := false, false
	previous := -1.
	for _, frame := range second.Frames {
		if frame.Sequence <= first.Sequence || frame.Captured <= previous {
			t.Fatal("history was duplicated or reordered")
		}
		previous = frame.Captured
		var state struct{ Positions []float64 }
		_ = json.Unmarshal(frame.State, &state)
		if state.Positions[2] == .18 {
			foundLift = true
		}
		if foundLift && state.Positions[2] == 0 {
			foundLanding = true
		}
	}
	if !foundLift || !foundLanding || maxActive.Load() != 1 {
		t.Fatalf("lift=%v landing=%v active=%d", foundLift, foundLanding, maxActive.Load())
	}
	other := context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{"bob", robotGatewayScopes})
	denied, _ := g.protocol.ListTools()["simulator_scene_pause"].Handler(other, callToolReq("simulator_scene_pause", map[string]any{"session_id": id}))
	if !denied.IsError {
		t.Fatal("another account paused the observer")
	}
	paused, _ := g.protocol.ListTools()["simulator_scene_pause"].Handler(ctx, callToolReq("simulator_scene_pause", map[string]any{"session_id": id}))
	if paused.IsError {
		t.Fatal("pause failed")
	}
	scene.history.mu.Lock()
	done := scene.history.done
	scene.history.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("pause did not stop capture")
		}
	}
	scene.history.mu.Lock()
	if len(scene.history.frames) != 0 {
		t.Error("pause retained hidden movement history")
	}
	scene.history.mu.Unlock()
	resumed := read(second.Sequence)
	if len(resumed.Frames) == 0 || resumed.Frames[0].Sequence <= second.Sequence {
		t.Fatal("resume did not capture a fresh pose")
	}
	scene.history.mu.Lock()
	done = scene.history.done
	scene.history.lastRead = time.Now().Add(-simulatorHistoryLease)
	scene.history.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle collector did not expire")
	}
}

func TestSimulatorPoseHistoryCloseCancelsCapture(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"scene_id":"fixture","epoch":1,"generation":1,"time":0,"positions":[0,0,0],"quaternions":[0,0,0,1]}`)
			return
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "g1")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "g1", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id := session["token"].(string)
	scene, _ := g.authorizedSimulatorScene(ctx, id)
	defer scene.cancel()
	if _, err := g.readSimulatorPoseHistory(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("capture did not start")
	}
	_, _ = g.protocol.ListTools()["simulator_scene_close"].Handler(ctx, callToolReq("simulator_scene_close", map[string]any{"session_id": id}))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("closing the viewer left an upstream read active")
	}
}
