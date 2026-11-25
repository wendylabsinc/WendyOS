package node

import (
	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
	"time"
)

// Structural fixture for the actual signed HTTP admission path. Sensor model
// reproduction and preparation freshness are tested by the incident app.
func incidentReviewTrigger(t *testing.T, s tasking.Spec) *tasking.TriggerRef {
	t.Helper()
	c := destinationHazardTrigger(t, s).Component
	c.Key.Type = missioncontrol.IncidentInspectionType
	c.Key.Entity = s.ControlContext.Context.RunID + "/incident-inspection"
	c.Provenance.AppVersion = "world-incident-inspection-v1"
	c.Provenance.ModelID = "incident-inspection-selection"
	c.Provenance.ModelVersion = "1"
	p := missioncontrol.IncidentSelection{Schema: missioncontrol.IncidentInspectionType, IncidentSHA256: strings.Repeat("a", 64), Contract: strings.Repeat("b", 64), InputDigest: strings.Repeat("c", 64), State: "consistent", Position: [3]float64{s.Target[0], .65, s.Target[2]}, Uncertainty: .1, ValidUntil: c.Timing.SampleTime.Add(c.TTL)}
	for i, kind := range []string{"gas", "visual"} {
		source := c.Key.Source
		source.Service = kind
		p.Supports = append(p.Supports, missioncontrol.IncidentSupport{Kind: kind, SHA256: strings.Repeat(string("de"[i]), 64), Source: source, Version: world.Version{Epoch: 1, Sequence: 1}, Captured: c.Timing.SampleTime, ValidUntil: p.ValidUntil})
	}
	c.Payload, _ = json.Marshal(p)
	trigger, e := tasking.TriggerFromComponent(c)
	if e != nil {
		t.Fatal(e)
	}
	return trigger
}
func TestIncidentInspectionSignedReviewAndStopBeforeDelivery(t *testing.T) {
	for _, mode := range []string{"normal", "stop-first", "target-tamper", "support-tamper", "denied-agent", "wrong-frame"} {
		t.Run(mode, func(t *testing.T) {
			r, op, s := nativeReviewFixture(t)
			s.Trigger = incidentReviewTrigger(t, s)
			original, _ := json.Marshal(s.Trigger)
			s.Target = [3]float64{.8, .24, .2}
			s.InspectionDestination = &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: strings.Repeat("f", 64)}
			parameters, _ := json.Marshal(missioncontrol.InspectParameters{Target: s.Target, TriggerSHA256: s.Trigger.SHA256, InspectionDestination: s.InspectionDestination})
			s.Proposal = nativeProposal(t, r, s, string(parameters))
			listed, e := r.f.n.cfg.Tasks.List()
			if e != nil || len(listed) != 0 {
				t.Fatal("proposal caused task admission", listed, e)
			}
			switch mode {
			case "stop-first":
				code, reply := call(t, r.f, op, "POST", "/v1/tasks/stop", "stop", instructionBody(t, r.f, op, "stop", StopRequest{TaskID: s.ID}))
				if code != 200 && code != 202 {
					t.Fatal(code, string(reply))
				}
			case "target-tamper":
				s.Target[0] += .1
			case "support-tamper":
				var p missioncontrol.IncidentSelection
				_ = json.Unmarshal(s.Trigger.Component.Payload, &p)
				p.Supports[0].SHA256 = strings.Repeat("0", 64)
				s.Trigger.Component.Payload, _ = json.Marshal(p)
				s.Trigger, e = tasking.TriggerFromComponent(s.Trigger.Component)
				if e != nil {
					t.Fatal(e)
				}
			case "wrong-frame":
				s.Frame.Revision = "other"
			}
			sender := op
			if mode == "denied-agent" {
				sender = r.agent
			}
			body := instructionBody(t, r.f, op, "mission", s)
			code, reply := call(t, r.f, sender, "POST", "/v1/tasks", "mission", body)
			if mode != "normal" && mode != "stop-first" {
				if code == 200 {
					t.Fatal("altered or agent-approved instruction admitted", string(reply))
				}
				return
			}
			if mode == "stop-first" {
				if code != 403 || !strings.Contains(string(reply), "task ID already bound to a different instruction") {
					t.Fatal("stop did not reject later delivery", code, string(reply))
				}
				listed, e = r.f.n.cfg.Tasks.List()
				if e != nil || len(listed) != 1 || string(listed[0].Status.Phase) != "canceled" || listed[0].Spec.Trigger != nil {
					t.Fatal("stop tombstone was replaced by an executable task", len(listed), e)
				}
				return
			}
			if code != 200 {
				t.Fatal(code, string(reply))
			}
			var accepted tasking.Record
			_ = json.Unmarshal(reply, &accepted)
			retained, _ := json.Marshal(accepted.Spec.Trigger)
			if !bytes.Equal(original, retained) {
				t.Fatal("selected trigger rewritten")
			}
			// The same signed body remains idempotent after the observation expires.
			r.now.Store(time.Now().Add(20 * time.Minute).UnixMilli())
			code, reply = call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
			if code != 200 {
				t.Fatal("exact retry", code, string(reply))
			}
			var again tasking.Record
			_ = json.Unmarshal(reply, &again)
			if !bytes.Equal(accepted.Proof, again.Proof) || tasking.SpecDigest(accepted.Spec) != tasking.SpecDigest(again.Spec) {
				t.Fatal("retry changed immutable task")
			}
			listed, e = r.f.n.cfg.Tasks.List()
			if e != nil || len(listed) != 1 {
				t.Fatal("duplicate admission", len(listed), e)
			}
		})
	}
}
