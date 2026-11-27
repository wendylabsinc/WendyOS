package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func TestPlanarInspectionDestinationBindsCanonicalConfigurationAndReplans(t *testing.T) {
	c, a, g := planarFixture(t)
	digest, err := PlanarConfigSHA256(c.Planar.config)
	if err != nil || digest != c.Planar.configSHA256 {
		t.Fatal(digest, err)
	}
	// Whitespace in the commissioned file is not part of the map identity.
	raw, _ := json.MarshalIndent(c.Planar.config, "", "  ")
	var decoded PlanarConfig
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("configuration decode")
	}
	again, err := PlanarConfigSHA256(decoded)
	if err != nil || digest != again {
		t.Fatal("canonical configuration changed with file formatting", err)
	}
	d := &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: digest}
	g.records[0].Spec.InspectionDestination = d
	g.decision.Record = g.records[0]
	before := g.records[0].Spec.Target
	if _, err = c.Tick(context.Background()); err != nil || a.commands != 1 {
		t.Fatal("matching descriptor did not use existing planner", err)
	}
	if before != g.records[0].Spec.Target {
		t.Fatal("executor retargeted reviewed destination")
	}
	// A selected point later found blocked is refused, not moved to a new cell.
	g.records[0].Spec.Target = [3]float64{3.5, .24, -3.5}
	g.decision.Record = g.records[0]
	if _, err = c.Tick(context.Background()); err == nil || a.commands != 1 {
		t.Fatal("blocked reviewed destination was silently replaced", err)
	}
}

func TestPlanarInspectionDestinationNeverUsesDefaultScalarAdapter(t *testing.T) {
	a := &fakeAdapter{state: sample()}
	r := record()
	r.Spec.Type = tasking.InspectType
	r.Spec.InspectionDestination = &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .2, MaxOffsetM: 2, PlanarConfigSHA256: strings.Repeat("a", 64)}
	g := &fakeGateway{records: []tasking.Record{r}, decision: tasking.Decision{Action: mission.Continue, Record: r}}
	c := &Controller{Adapter: a, Gateway: g}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
		t.Fatal("explicit planar instruction reached default scalar driver", err)
	}
	// The same legacy target still follows the scalar path with no descriptor.
	g.records[0].Spec.InspectionDestination = nil
	g.decision.Record = g.records[0]
	if _, err := c.Tick(context.Background()); err != nil || a.commands != 1 {
		t.Fatal("legacy scalar behavior changed", err)
	}
}

func TestPlanarInspectionDestinationRejectsDifferentMapFloorAndDescriptor(t *testing.T) {
	for _, change := range []string{"map", "floor", "mode", "offset", "robot", "reservation"} {
		t.Run(change, func(t *testing.T) {
			c, a, g := planarFixture(t)
			d := &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: c.Planar.configSHA256}
			switch change {
			case "map":
				d.PlanarConfigSHA256 = strings.Repeat("0", 64)
			case "floor":
				d.FloorY = .25
			case "mode":
				d.Mode = "unbounded"
			case "offset":
				d.MaxOffsetM = 6
			case "robot":
				config := c.Planar.config
				config.RobotID = "iris"
				d.PlanarConfigSHA256, _ = PlanarConfigSHA256(config)
			case "reservation":
				config := c.Planar.config
				config.Reservations = append(config.Reservations, nil)
				d.PlanarConfigSHA256, _ = PlanarConfigSHA256(config)
			}
			g.records[0].Spec.InspectionDestination = d
			g.decision.Record = g.records[0]
			if _, err := c.Tick(context.Background()); err == nil || a.commands != 0 {
				t.Fatal("different planar configuration moved", err)
			}
		})
	}
}
