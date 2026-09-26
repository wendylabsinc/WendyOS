package models

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func TestParseDetection(t *testing.T) {
	rec := data.ApplicationRecord{Version: 1, Type: "event", Name: RecordEntered, Model: "d-cpu",
		Attributes: map[string]any{"class": "person", "confidence": 0.91, "track_id": float64(12),
			"box": map[string]any{"x": 0.1, "y": 0.2, "width": 0.3, "height": 1.4}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: 77}}}
	at := time.Unix(100, 0)
	e, err := parseDetection(rec, at)
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != EventEntered || e.Class != "person" || e.Confidence != float32(0.91) || e.TrackID != 12 {
		t.Fatalf("event = %+v", e)
	}
	if e.Box.Height != 1 {
		t.Fatalf("box height %v was not clamped to the frame", e.Box.Height)
	}
	if e.SourceID != "v4l2:/dev/video0" || e.SampleID != 77 || !e.Time.Equal(at) {
		t.Fatalf("event provenance = %+v", e)
	}
}

func TestParseDetectionRejectsBadRecords(t *testing.T) {
	cases := map[string]data.ApplicationRecord{
		"no class":           {Name: RecordEntered, Attributes: map[string]any{"confidence": 0.5}},
		"confidence above 1": {Name: RecordEntered, Attributes: map[string]any{"class": "person", "confidence": 1.5}},
		"not a detection":    {Name: RecordStatus, Attributes: map[string]any{"class": "person", "confidence": 0.5}},
	}
	for name, rec := range cases {
		if _, err := parseDetection(rec, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseStatus(t *testing.T) {
	st, err := parseStatus(data.ApplicationRecord{Name: RecordStatus, Attributes: map[string]any{
		"state": "ready", "processed_fps": 9.5, "latency_p50_ms": 31.0, "frames_skipped": float64(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != HostReady || st.Stats.ProcessedFPS != 9.5 || st.Stats.LatencyP50Ms != 31 || st.Stats.FramesSkipped != 4 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := parseStatus(data.ApplicationRecord{Name: RecordStatus, Attributes: map[string]any{"state": "napping"}}); err == nil {
		t.Fatal("accepted an unknown host state")
	}
}
