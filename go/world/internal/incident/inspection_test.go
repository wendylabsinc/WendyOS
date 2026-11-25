package incident

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
	"time"
)

func TestInspectionRetainsSelectionAndRejectsInvalidContexts(t *testing.T) {
	cfg, g, v := fixture(t)
	s, _ := New(cfg)
	r, e := s.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2))
	if e != nil {
		t.Fatal(e)
	}
	c, e := InspectionSelection(cfg, "east", world.Version{Epoch: 1, Sequence: 3}, r, cfg.Gas.Instant(2.3))
	if e != nil {
		t.Fatal(e)
	}
	trigger, e := tasking.TriggerFromComponent(c)
	if e != nil {
		t.Fatal(e)
	}
	target, e := missioncontrol.NativeTriggerTarget(c, .24)
	if e != nil || target != r.Visual.Claim.Result.Spatial.PositionM {
		t.Fatal(target, e)
	}
	if !c.Timing.SampleTime.Add(c.TTL).Equal(r.ValidUntil) {
		t.Fatal("source deadline changed")
	}
	// The retained historical trigger remains immutable as new captures change the view.
	newer := visualFixture(t, cfg, 1.4, 2.4, 2)
	_, _ = s.Step([]world.Component{g, newer}, cfg.Gas.Instant(2.5))
	if trigger.Validate(cfg.Visual.Site, cfg.Visual.Frame) != nil {
		t.Fatal("historical trigger invalidated")
	}
	if _, e = InspectionSelection(cfg, "east", world.Version{Epoch: 1, Sequence: 4}, r, r.ValidUntil); e == nil {
		t.Fatal("expired preparation")
	}
	r.State = "conflicting"
	if _, e = InspectionSelection(cfg, "east", world.Version{Epoch: 1, Sequence: 4}, r, cfg.Gas.Instant(2.3)); e == nil {
		t.Fatal("mutated or conflicting result accepted")
	}
	for _, name := range []string{"position-null", "deadline", "source", "run", "state", "duplicate-support", "duplicate-ancestry", "tamper-target"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(c)
			var bad world.Component
			_ = json.Unmarshal(raw, &bad)
			var p map[string]any
			_ = json.Unmarshal(bad.Payload, &p)
			switch name {
			case "position-null":
				p["position_m"] = []any{nil, 0, 4}
			case "deadline":
				p["valid_until"] = r.ValidUntil.Add(time.Second)
			case "source":
				p["supports"].([]any)[0].(map[string]any)["source"] = map[string]string{}
			case "run":
				bad.Provenance.Inputs[0].Simulation.RunID = "other"
			case "state":
				p["state"] = "gas_only"
			case "duplicate-support":
				p["supports"].([]any)[1] = p["supports"].([]any)[0]
			case "duplicate-ancestry":
				bad.Provenance.Inputs[1] = bad.Provenance.Inputs[0]
			case "tamper-target":
				if missioncontrol.ValidateNativeInspectionTarget(c, [3]float64{100, .24, 100}, nil) == nil {
					t.Fatal("retarget accepted")
				}
				return
			}
			bad.Payload, _ = json.Marshal(p)
			if _, e := missioncontrol.IncidentTriggerTarget(bad); e == nil {
				t.Fatal("malformed trigger accepted")
			}
		})
	}
}
