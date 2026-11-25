package sensing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func wireSample() Sample {
	s := sampleFixture()
	s.SensorID = "camera-a"
	s.ID = "run/camera-a/1"
	s.Kind = "camera"
	s.Gas = nil
	s.Evidence = &EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/jpeg", Size: 100}
	s.Camera = &Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Second)}
	s.SensorLink = &SensorLink{Protocol: "wendylite.v1.sensorlink", ChannelID: 1, FrameSequence: 0, TimestampUS: 1700000000250000, Flags: 0, FrameSHA256: s.Evidence.SHA256, SourceClock: "unix-us", AnchorUnixUS: 1700000000000000, ClockErrorUS: 1000}
	return s
}

func TestSensorLinkMetadataRequiresExplicitZeroValuesAndBoundTime(t *testing.T) {
	s := wireSample()
	raw, _ := json.Marshal(s)
	if _, err := Decode(raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"frame_sequence", "flags", "timestamp_us", "clock_error_us", "anchor_unix_us"} {
		for _, null := range []bool{false, true} {
			var outer map[string]json.RawMessage
			json.Unmarshal(raw, &outer)
			var link map[string]json.RawMessage
			json.Unmarshal(outer["sensorlink"], &link)
			if null {
				link[field] = json.RawMessage("null")
			} else {
				delete(link, field)
			}
			outer["sensorlink"], _ = json.Marshal(link)
			bad, _ := json.Marshal(outer)
			if _, err := Decode(bad); err == nil {
				t.Fatal("accepted missing/null wire metadata", field, null)
			}
		}
	}
	for _, change := range []func(*Sample){func(s *Sample) { s.SensorLink.TimestampUS++ }, func(s *Sample) { s.SensorLink.FrameSHA256 = strings.Repeat("c", 64) }, func(s *Sample) { s.SensorLink.SourceClock = "receipt-time" }, func(s *Sample) { s.SensorLink.ClockErrorUS = 0 }, func(s *Sample) { s.Evidence.ContentType = "image/png" }, func(s *Sample) { s.SensorLink.Flags = 2 }} {
		bad := wireSample()
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("accepted incompatible wire lineage")
		}
	}
}
