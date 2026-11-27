package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func request() gasfield.Request {
	c := &gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "test", ScenarioVersion: "v1", ClockDomain: "simulation:test", Seed: 7}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}
	for i, p := range [][2]float64{{-1, -1}, {1, -1}, {-1, 1}, {1, 1}} {
		id := string(rune('a' + i))
		c.Sensors = append(c.Sensors, gasfield.Sensor{ID: id, Source: world.Source{Device: id, App: "gas", Service: id, Stream: "ppm"}, Position: p, Calibration: "c1", SensorType: "synthetic"})
	}
	return gasfield.Request{Config: c, Now: 0, Assumptions: gasfield.Assumptions{Revision: "a1", WindRadians: .2, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Observations: []gasfield.Observation{}}
}
func TestServeActualProtocolAndFailureRecovery(t *testing.T) {
	req := request()
	raw, _ := json.Marshal(req)
	var out bytes.Buffer
	if err := serve(strings.NewReader("{broken}\n"+string(raw)+"\n"+`{"now_s":0}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses %d", len(lines))
	}
	var result gasfield.Result
	if err := json.Unmarshal([]byte(lines[1]), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "unavailable" || result.Total != 4 {
		t.Fatalf("valid request failed: %s", lines[1])
	}
	for _, i := range []int{0, 2} {
		var response map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &response); err != nil || response["error"] == nil {
			t.Fatalf("missing refusal %s", lines[i])
		}
	}
}
func TestServeRejectsOversizedAndExtraFields(t *testing.T) {
	var out bytes.Buffer
	if err := serve(strings.NewReader(strings.Repeat(" ", 300<<10)+"\n"), &out); err == nil {
		t.Fatal("unbounded input admitted")
	}
	req := request()
	raw, _ := json.Marshal(req)
	withExtra := strings.Replace(string(raw), `"wind_radians":0.2`, `"wind_radians":0.2,"truth_source":[0,0]`, 1)
	out.Reset()
	if err := serve(strings.NewReader(withExtra+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "error") {
		t.Fatal("unknown truth field accepted")
	}
}

func TestNullCoordinatesRejectBeforeSessionInitialization(t *testing.T) {
	req := request()
	raw, _ := json.Marshal(req)
	for _, pair := range [][2]string{
		{`"position_xz":[-1,-1]`, `"position_xz":[null,-1]`},
		{`"position_xz":[-1,-1]`, `"position_xz":[-1,null]`},
		{`"min_xz":[-2,-2]`, `"min_xz":[null,-2]`},
		{`"max_xz":[2,2]`, `"max_xz":[null,2]`},
		{`"seed":7`, `"seed":null`},
		{`"seed":7,`, ``},
	} {
		invalid := strings.Replace(string(raw), pair[0], pair[1], 1)
		if invalid == string(raw) {
			t.Fatal("fixture replacement missed")
		}
		var out bytes.Buffer
		if err := serve(strings.NewReader(invalid+"\n"+string(raw)+"\n"), &out); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], "error") {
			t.Fatalf("null coordinate admitted: %s", out.String())
		}
		var result gasfield.Result
		if err := json.Unmarshal([]byte(lines[1]), &result); err != nil || result.Total != 4 || result.State != "unavailable" {
			t.Fatalf("rejected input initialized immutable session: %s", lines[1])
		}
	}
}
