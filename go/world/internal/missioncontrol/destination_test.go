package missioncontrol

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestInspectionDestinationPreservesObservedHazardAndLegacyTarget(t *testing.T) {
	c := nativeHazard(t)
	before, _ := json.Marshal(c)
	observed, err := NativeTriggerTarget(c, .24)
	if err != nil {
		t.Fatal(err)
	}
	d := InspectionDestination{1, "planar_nearby", .24, 2, strings.Repeat("a", 64)}
	target := [3]float64{observed[0] + 1, d.FloorY, observed[2]}
	if err = ValidateNativeInspectionTarget(c, target, &d); err != nil {
		t.Fatal(err)
	}
	if ValidateNativeInspectionTarget(c, target, nil) == nil || ValidateNativeInspectionTarget(c, observed, nil) != nil {
		t.Fatal("descriptor omission changed exact observed-target behavior")
	}
	for name, invalid := range map[string][3]float64{
		"beyond-offset": {observed[0] + 2.01, .24, observed[2]},
		"wrong-floor":   {observed[0], .25, observed[2]},
		"nonfinite":     {math.Inf(1), .24, observed[2]},
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateNativeInspectionTarget(c, invalid, &d) == nil {
				t.Fatal("invalid destination accepted")
			}
		})
	}
	after, _ := json.Marshal(c)
	if !bytes.Equal(before, after) {
		t.Fatal("projection modified original component")
	}
}

func TestInspectionDestinationWireIsExplicitAndProposalBindsEveryField(t *testing.T) {
	d := InspectionDestination{1, "planar_nearby", .24, 2, strings.Repeat("a", 64)}
	raw, _ := json.Marshal(d)
	want := `{"version":1,"mode":"planar_nearby","floor_y":0.24,"max_offset_m":2,"planar_config_sha256":"` + strings.Repeat("a", 64) + `"}`
	if string(raw) != want {
		t.Fatalf("descriptor order changed: %s", raw)
	}
	legacy, _ := json.Marshal(InspectParameters{Target: [3]float64{1, .2, 0}})
	if string(legacy) != `{"target":[1,0.2,0]}` {
		t.Fatalf("legacy proposal gained a field: %s", legacy)
	}
	p := InspectParameters{Target: [3]float64{1, .24, 0}, TriggerSHA256: strings.Repeat("b", 64), InspectionDestination: &d}
	encoded, _ := json.Marshal(p)
	decoded, err := DecodeInspectParameters(encoded)
	if err != nil || decoded.Target != p.Target || !EqualInspectionDestination(decoded.InspectionDestination, &d) {
		t.Fatal(decoded, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing-floor": func(m map[string]any) { delete(m, "floor_y") },
		"null-floor":    func(m map[string]any) { m["floor_y"] = nil },
		"zero-radius":   func(m map[string]any) { m["max_offset_m"] = 0 },
		"large-radius":  func(m map[string]any) { m["max_offset_m"] = 5.01 },
		"mode":          func(m map[string]any) { m["mode"] = "planar" },
		"digest":        func(m map[string]any) { m["planar_config_sha256"] = "invalid" },
		"version":       func(m map[string]any) { m["version"] = 2 },
		"extra":         func(m map[string]any) { m["route_clear"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			mutate(fields)
			changed, _ := json.Marshal(fields)
			var got InspectionDestination
			if json.Unmarshal(changed, &got) == nil {
				t.Fatal("invalid descriptor accepted")
			}
		})
	}
	for _, value := range [][]byte{[]byte("null"), append([]byte(`{"floor_y":1,`), raw[1:]...)} {
		var got InspectionDestination
		if json.Unmarshal(value, &got) == nil {
			t.Fatal("ambiguous descriptor accepted")
		}
	}
	p.TriggerSHA256 = ""
	encoded, _ = json.Marshal(p)
	if _, err = DecodeInspectParameters(encoded); err == nil {
		t.Fatal("destination without selected original digest accepted")
	}
	if EqualInspectionDestination(nil, &d) || !EqualInspectionDestination(nil, nil) {
		t.Fatal("optional descriptor equality changed")
	}
}
