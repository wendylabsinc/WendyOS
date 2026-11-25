package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func demoRequest(t *testing.T, sequence uint64) []byte {
	t.Helper()
	r := request{RunID: "office-demo", NowNS: int64(time.Second), Frame: world.FrameRef{ID: "floor", Revision: "1"}, Media: map[string][]byte{}}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("office-demo/%s/%d", id, sequence), SiteID: "studio-01", Frame: r.Frame, SensorID: id, Sequence: sequence, SampleNS: int64(time.Second), RunID: r.RunID, ScenarioVersion: "office", PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{0, 0, 0}, UncertaintyM: .01, CalibrationRevision: "c1"}
		switch id {
		case "camera-a", "camera-b":
			s.Kind = "camera"
			s.Camera = &sensing.Camera{Width: 4, Height: 4, FX: 3, FY: 3, CX: 2, CY: 2, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Second)}
			s.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 16}
		case "gas-1":
			s.Kind = "gas"
			s.Gas = &sensing.Gas{ValuePPM: 100, UncertaintyPPM: 1, Species: "synthetic"}
		case "audio-1":
			s.Kind = "audio"
			s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			s.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), ContentType: "audio/wav", Size: 4044}
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		r.Samples = append(r.Samples, raw)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestPersistentNDJSONKeepsNativeSequenceAndRecoversWithNewSample(t *testing.T) {
	one, two := demoRequest(t, 1), demoRequest(t, 2)
	input := bytes.Join([][]byte{one, one, two}, []byte("\n"))
	var output bytes.Buffer
	if err := serve(bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for i := 0; i < 3; i++ {
		var result fusion.Result
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.State != fusion.InsufficientEvidence {
			t.Fatal(result)
		}
		if strings.HasPrefix(result.Reason, "demo input refused:") != (i == 1) {
			t.Fatalf("line%d: %+v", i, result)
		}
	}
}
func TestDemoClockAndScopeCannotResetConverters(t *testing.T) {
	for _, field := range []string{"run_id", "frame", "now_ns"} {
		t.Run(field, func(t *testing.T) {
			e := new(evaluator)
			if _, err := e.evaluate(demoRequest(t, 1)); err != nil {
				t.Fatal(err)
			}
			var changed map[string]any
			json.Unmarshal(demoRequest(t, 2), &changed)
			switch field {
			case "run_id":
				changed[field] = "another"
			case "frame":
				changed[field] = map[string]string{"id": "other", "revision": "1"}
			case "now_ns":
				changed[field] = 0
			}
			raw, _ := json.Marshal(changed)
			if _, err := e.evaluate(raw); err == nil {
				t.Fatal("reset accepted")
			}
		})
	}
}
