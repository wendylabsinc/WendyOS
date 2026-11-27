package selection

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
	"time"
)

func TestRegionStrictGeometryAndConservativeUnknown(t *testing.T) {
	r := Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{1, 1}}
	c := world.Component{Key: world.Key{Type: "wendy.hazard.v1"}, Frame: r.Frame}
	for _, tc := range []struct{ payload, class string }{
		{`{"position_m":[1.2,0,0.5],"uncertainty_m":0.3}`, "located"},
		{`{"position_m":[1.2,0,0.5],"uncertainty_m":0.1}`, "outside"},
		{`{"position_m":[0,0,0],"uncertainty_ppm":0}`, "unlocated"},
		{`{"position_m":[null,0,0],"uncertainty_m":0}`, "unlocated"},
		{`{"position_m":[0,0,0],"uncertainty_m":null}`, "unlocated"},
		{`{"position_m":[0,0,0],"uncertainty_m":-1}`, "unlocated"},
		{`{"position_m":[0,0,0],"uncertainty_m":0,"uncertainty_m":2}`, "unlocated"},
	} {
		c.Payload = json.RawMessage(tc.payload)
		got := r.Classify(c)
		if got.Class != tc.class {
			t.Fatalf("%s: %+v", tc.payload, got)
		}
		if got.Class == "unlocated" && len(got.PositionM) > 0 {
			t.Fatal("unknown projected")
		}
	}
	for _, raw := range []string{`{"frame":{"id":"f","revision":"1"},"max_xz":[1,1]}`, `{"frame":{"id":"f","revision":"1"},"min_xz":[null,0],"max_xz":[1,1]}`, `{"frame":{"id":"f","revision":"1"},"min_xz":[0,0],"max_xz":[0,1]}`, `{"frame":{"id":"f","revision":"1"},"min_xz":[0,0,0],"max_xz":[1,1]}`} {
		if json.Unmarshal([]byte(raw), &r) == nil {
			t.Fatal(raw)
		}
	}
	if r.Frame.ID != "floor" {
		t.Fatal("invalid decode mutated receiver")
	}
}
func TestRegionSelectionScopeAndClone(t *testing.T) {
	r := &Region{Frame: world.FrameRef{ID: "f", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}
	s := Selection{Types: []string{"wendy.hazard.v1"}, Region: r}
	n, err := s.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	r.MaxXZ[0] = 99
	if n.Region.MaxXZ[0] != 2 {
		t.Fatal("aliased region")
	}
	for _, s := range []Selection{{Region: r}, {Types: []string{"arbitrary"}, Region: r}, {Types: []string{"wendy.hazard.v1"}, Region: r, Frames: []world.FrameRef{r.Frame}}} {
		if _, err := s.Normalize(); err == nil {
			t.Fatal("accepted invalid spatial scope")
		}
	}
}

func TestRegionRejectsConflictingSensorFrameBinding(t *testing.T) {
	frame := world.FrameRef{ID: "floor", Revision: "1"}
	source := world.Source{Device: "d", App: "a", Service: "gas-1", Stream: "s"}
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/gas-1/1", SiteID: "site", Frame: frame, SensorID: "gas-1", Kind: "gas", Sequence: 1, RunID: "run", ScenarioVersion: "1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{1, 0, 1}, UncertaintyM: .1, CalibrationRevision: "1", Gas: &sensing.Gas{ValuePPM: 10, UncertaintyPPM: 1, Species: "fixture"}}
	converter, err := sensing.New(sensing.Config{Tenant: "t", Site: "site", RunID: "run", Frame: frame, Source: source, Epoch: 1, Anchor: time.Now(), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sample)
	c, err := converter.Convert(raw)
	if err != nil {
		t.Fatal(err)
	}
	region := Region{Frame: frame, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}
	if region.Classify(c).Class != "located" {
		t.Fatal("valid sensor unlocated")
	}
	sample.Frame.Revision = "2"
	c.Payload, _ = json.Marshal(sample)
	if region.Classify(c).Class != "unlocated" {
		t.Fatal("contradictory sensor frame plotted")
	}
}
