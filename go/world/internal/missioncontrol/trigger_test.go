package missioncontrol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func nativeHazard(t *testing.T) world.Component {
	t.Helper()
	// Existing synthetic production fixture, read as JSON without importing the
	// nested production module. Its complete original result bytes are retained.
	raw, err := os.ReadFile("../../production/replication/testdata/hazard-lidar.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Entity, ComponentType, Stream string
		Frame                         world.FrameRef
		Timing                        world.Timing
		TTL                           time.Duration
		Payload                       []byte
		Provenance                    world.Provenance
	}
	// Publication uses snake-case names.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for name, out := range map[string]any{"entity": &p.Entity, "component_type": &p.ComponentType, "stream": &p.Stream, "frame": &p.Frame, "timing": &p.Timing, "ttl_ns": &p.TTL, "payload": &p.Payload, "provenance": &p.Provenance} {
		if err := json.Unmarshal(fields[name], out); err != nil {
			t.Fatal(name, err)
		}
	}
	var r triggerResult
	if json.Unmarshal(p.Payload, &r) != nil || len(r.Inputs) == 0 {
		t.Fatal("derived fixture")
	}
	key := r.Inputs[0].Key
	return world.Component{Key: world.Key{Tenant: key.Tenant, Site: key.Site, Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: key.Source.Device, App: "com.example.fusion", Service: "fusion", Stream: p.Stream}}, Version: world.Version{Epoch: 7, Sequence: 3}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Payload: p.Payload, Provenance: p.Provenance}
}

func TestNativeHazardTargetPreservesFullResultAndBounds(t *testing.T) {
	c := nativeHazard(t)
	before, _ := json.Marshal(c)
	var result triggerResult
	_ = json.Unmarshal(c.Payload, &result)
	var body struct {
		Position [3]float64 `json:"position_m"`
	}
	_ = json.Unmarshal(result.ResultJSON, &body)
	target, err := NativeTriggerTarget(c, 99)
	if err != nil || target != body.Position {
		t.Fatal(target, err)
	}
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("original component changed")
	}
}

func TestNativeTriggerShapeRefusesAmbiguousOrChangedContext(t *testing.T) {
	for _, mode := range []string{"schema", "reference", "provenance", "type", "short-vector", "null-vector", "duplicate", "unknown", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c := nativeHazard(t)
			var r triggerResult
			_ = json.Unmarshal(c.Payload, &r)
			var body map[string]json.RawMessage
			_ = json.Unmarshal(r.ResultJSON, &body)
			switch mode {
			case "schema":
				r.ResultSchema = "wendy.unknown.v1"
			case "reference":
				r.Inputs[0].Key.Site = "other"
			case "provenance":
				c.Provenance.Inputs[0].ID = "sha256:" + strings.Repeat("0", 64)
			case "type":
				c.Key.Type = "wendy.hazard.v1"
			case "short-vector":
				body["position_m"] = []byte("[1,2]")
			case "null-vector":
				body["position_m"] = []byte("[1,null,2]")
			case "unknown":
				body["command"] = []byte(`"pursue"`)
			case "deadline":
				c.TTL += time.Second
			}
			r.ResultJSON, _ = json.Marshal(body)
			if mode == "duplicate" {
				r.ResultJSON = append(append([]byte(`{"position_m":[1,2,3],`), r.ResultJSON[1:]...), ' ')
			}
			c.Payload, _ = json.Marshal(r)
			if _, err := NativeTriggerTarget(c, 0); err == nil {
				t.Fatal("changed context accepted")
			}
		})
	}
}

func TestInspectProposalDigestIsOptionalAndNeverDropsOtherFields(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, raw := range []string{`{"target":[1,0.2,3]}`, `{"trigger_sha256":"` + hash + `","target":[1,0.2,3]}`} {
		p, err := DecodeInspectParameters([]byte(raw))
		if err != nil || p.Target != [3]float64{1, .2, 3} {
			t.Fatal(p, err)
		}
	}
	for _, raw := range []string{`{"target":[1,2,3],"trigger_sha256":null}`, `{"target":[1,2,3],"trigger_sha256":""}`, `{"target":[1,2,3],"trigger_sha256":"A"}`, `{"target":[1,2,3],"trigger":{}}`, `{"target":[1,2,3],"trigger_sha256":"` + hash + `","trigger_sha256":"` + hash + `"}`, `{"target":[1,2,3],"speed":1}`} {
		if _, err := DecodeInspectParameters([]byte(raw)); err == nil {
			t.Fatal("ambiguous proposal accepted", raw)
		}
	}
	if _, err := DecodeTarget([]byte(`{"target":[1,2,3],"trigger_sha256":"` + hash + `"}`)); err == nil {
		t.Fatal("target-only decoder silently discarded trigger")
	}
}
