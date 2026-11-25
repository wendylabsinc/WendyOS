package executor

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type planarFake struct {
	fakeAdapter
	vector [3]float64
}

func (a *planarFake) CommandPlanar(_ context.Context, _ string, _ uint64, v [3]float64, lease time.Duration) error {
	a.commands++
	a.vector = v
	if lease != commandLease {
		panic("command lease changed")
	}
	return nil
}

func planarFixture(t *testing.T) (*Controller, *planarFake, *fakeGateway) {
	t.Helper()
	s := sample()
	s.SchemaVersion, s.RobotID = PlanarStateSchema, "atlas"
	s.Position = [3]float64{.5, .24, -.5}
	s.RunID, s.Site = "office-run", "studio-01"
	s.Provenance = StateProvenance{Mode: "simulated", PackageSHA256: strings.Repeat("a", 64)}
	a := &planarFake{fakeAdapter: fakeAdapter{state: s}}
	r := record()
	r.Spec.Type = tasking.InspectType
	r.Spec.Target = [3]float64{6.5, .24, -6.5}
	r.Spec.Area = tasking.Bounds{Min: [3]float64{0, .1, -8}, Max: [3]float64{8, .4, 0}}
	r.Spec.ControlContext = &missioncontrol.SignedContext{Context: missioncontrol.Context{RunID: s.RunID, Site: s.Site, Frame: s.Frame, PackageSHA256: s.Provenance.PackageSHA256}}
	nav, err := NewPlanarNavigation(PlanarConfig{RobotID: "atlas", FloorY: .24, Grid: collabdemo.Grid{Width: 8, Height: 8, Resolution: 1, Blocked: []collabdemo.Cell{{3, 3}}}})
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{records: []tasking.Record{r}, decision: tasking.Decision{Action: mission.Continue, Record: r}}
	c := &Controller{Adapter: a, Gateway: g, Planar: nav}
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, a, g
}

func TestPlanarCurrentDecisionFollowsOfficeRouteInWorldCoordinates(t *testing.T) {
	c, a, g := planarFixture(t)
	for step := 0; step < 2500; step++ {
		a.state.SampleUnixMS = time.Now().UnixMilli()
		if _, err := c.Tick(context.Background()); err != nil {
			t.Fatal(step, err)
		}
		if a.vector[1] != 0 || math.Hypot(a.vector[0], a.vector[2]) > g.records[0].Spec.SpeedMPS+1e-12 {
			t.Fatal("invalid planar route velocity", a.vector)
		}
		a.state.Position[0] += a.vector[0] * .02
		a.state.Position[2] += a.vector[2] * .02
		x, y := a.state.Position[0], -a.state.Position[2]
		if x >= 3 && x <= 4 && y >= 3 && y <= 4 {
			t.Fatal("route crossed blocked grid cell")
		}
		if math.Hypot(x-6.5, y-6.5) <= g.records[0].Spec.ToleranceM {
			break
		}
		if step == 2499 {
			t.Fatal("did not reach the fixed target", a.state.Position)
		}
	}
	if a.commands == 0 || g.evaluations != a.commands {
		t.Fatal("commands bypassed current task decisions")
	}
	before := a.commands
	g.decision.Action = mission.Hold
	if _, err := c.Tick(context.Background()); err != nil || a.commands != before || a.stops != 1 {
		t.Fatal("hold did not brake", err)
	}
	if _, err := collabdemo.PlanRoute(c.Planar.config.Grid, collabdemo.Vec{.5, .5}, collabdemo.Vec{3.5, 3.5}, nil); err == nil {
		t.Fatal("blocked target was moved to a different cell")
	}
}

func TestPlanarRefusesWrongActuatorScopeAndUnsupportedTasks(t *testing.T) {
	for _, mode := range []string{"default", "robot", "floor", "package", "supervised", "uncommissioned", "outside", "missing-planar-adapter"} {
		t.Run(mode, func(t *testing.T) {
			c, a, g := planarFixture(t)
			switch mode {
			case "default":
				c.Planar = nil
			case "robot":
				a.state.RobotID = "iris"
			case "floor":
				a.state.Position[1] = .2
			case "package":
				a.state.Provenance.PackageSHA256 = strings.Repeat("b", 64)
			case "supervised":
				g.records[0].Spec.Mode = mission.Supervised
			case "uncommissioned":
				g.records[0].Spec.ControlContext = nil
			case "outside":
				g.records[0].Spec.Target[0] = 9
			case "missing-planar-adapter":
				c.Adapter = &a.fakeAdapter
			}
			g.decision.Record = g.records[0]
			if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
				t.Fatal("unsupported planar operation moved", err)
			}
		})
	}
}

func planarWire(t *testing.T, state State) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	fields["commanded_velocity_mps"], _ = json.Marshal(state.PlanarCommandedVelocity)
	raw, _ = json.Marshal(fields)
	return raw
}

func TestPlanarHTTPUsesSeparateVectorContractAndStrictState(t *testing.T) {
	_, a, _ := planarFixture(t)
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		want := strings.Repeat("c", 32)
		if r.Method == "GET" {
			want = strings.Repeat("r", 32)
		}
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("wrong read/control adapter token")
		}
		if r.URL.Path == "/v1/inspection/command" {
			var body struct {
				Velocity [3]float64 `json:"velocity_mps"`
				Lease    int64      `json:"lease_ms"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Velocity != [3]float64{.2, 0, -.3} || body.Lease != 300 {
				t.Error("vector or original lease changed", body)
			}
		}
		_, _ = w.Write(planarWire(t, a.state))
	}))
	defer server.Close()
	h, err := NewPlanarHTTPAdapter(server.URL, strings.Repeat("r", 32), strings.Repeat("c", 32), "atlas", .24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = h.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = h.CommandPlanar(context.Background(), "session", 1, [3]float64{.2, 0, -.3}, commandLease); err != nil {
		t.Fatal(err)
	}
	if err = h.Stop(context.Background(), "session", 2); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 || paths[0] != "/v1/inspection/session" || paths[1] != "/v1/inspection/state" || paths[3] != "/v1/inspection/stop" {
		t.Fatal(paths)
	}
	if err = h.Command(context.Background(), "session", 3, .1, commandLease); err == nil {
		t.Fatal("scalar fallback allowed")
	}
	good := planarWire(t, a.state)
	if _, err = decodeState(good); err == nil {
		t.Fatal("default decoder accepted planar wire")
	}
	for _, bad := range []string{"0", "[0,0]", "[0,null,0]", "[0,1,0]", "[1,0,0]"} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(good, &fields)
		fields["commanded_velocity_mps"] = json.RawMessage(bad)
		raw, _ := json.Marshal(fields)
		if _, err = h.read.decode(raw); err == nil {
			t.Fatal("accepted malformed planar state", bad)
		}
	}
}

func TestPlanarBoundsRetainExistingCommandBudget(t *testing.T) {
	c, a, g := planarFixture(t)
	_ = c
	a.state.Position[2] = -7.99
	a.state.PlanarCommandedVelocity[2] = -.4
	if _, brake, err := boundedPlanarVelocity(a.state, g.records[0].Spec, [3]float64{.1, 0, -.1}, time.Now()); err != nil || !brake {
		t.Fatal("outward prior command did not brake", err)
	}
	a.state.PlanarCommandedVelocity = [3]float64{}
	a.state.SampleUnixMS -= 300
	if _, _, err := boundedPlanarVelocity(a.state, g.records[0].Spec, [3]float64{}, time.Now()); err == nil {
		t.Fatal("stale planar state accepted")
	}
}
