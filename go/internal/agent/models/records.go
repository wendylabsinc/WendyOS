package models

import (
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// Record names a model host sends as "event" records on its data socket. The
// host side of this contract is go/modelhost; keep the two in step.
const (
	RecordEntered = "model.entered"
	RecordLeft    = "model.left"
	RecordStatus  = "model.status"
)

// States a host reports in model.status records.
const (
	HostBuildingEngine = "building_engine"
	HostReady          = "ready"
	HostFailed         = "failed"
)

// HostStatus is a parsed model.status record.
type HostStatus struct {
	State  string
	Reason string
	Stats  Stats
}

func parseStatus(rec data.ApplicationRecord) (HostStatus, error) {
	st := HostStatus{State: attrString(rec.Attributes, "state"), Reason: attrString(rec.Attributes, "reason")}
	switch st.State {
	case HostBuildingEngine, HostReady, HostFailed:
	default:
		return HostStatus{}, fmt.Errorf("model.status has unknown state %q", st.State)
	}
	st.Stats.ProcessedFPS = float32(attrFloat(rec.Attributes, "processed_fps"))
	st.Stats.LatencyP50Ms = float32(attrFloat(rec.Attributes, "latency_p50_ms"))
	if skipped := attrFloat(rec.Attributes, "frames_skipped"); skipped > 0 {
		st.Stats.FramesSkipped = uint64(skipped)
	}
	return st, nil
}

// parseDetection turns a model.entered or model.left record into an Event
// received at `at`. Sequence stays zero; the instance's ring assigns it.
func parseDetection(rec data.ApplicationRecord, at time.Time) (Event, error) {
	e := Event{Time: at}
	switch rec.Name {
	case RecordEntered:
		e.Type = EventEntered
	case RecordLeft:
		e.Type = EventLeft
	default:
		return Event{}, fmt.Errorf("record %q is not a detection", rec.Name)
	}
	if e.Class = attrString(rec.Attributes, "class"); e.Class == "" {
		return Event{}, fmt.Errorf("%s has no class", rec.Name)
	}
	confidence := attrFloat(rec.Attributes, "confidence")
	if math.IsNaN(confidence) || confidence < 0 || confidence > 1 {
		return Event{}, fmt.Errorf("%s confidence %v is outside 0..1", rec.Name, confidence)
	}
	e.Confidence = float32(confidence)
	if track := attrFloat(rec.Attributes, "track_id"); track > 0 {
		e.TrackID = uint64(track)
	}
	if box, ok := rec.Attributes["box"].(map[string]any); ok {
		e.Box = Box{X: unit(box, "x"), Y: unit(box, "y"), Width: unit(box, "width"), Height: unit(box, "height")}
	}
	if len(rec.Inputs) > 0 {
		e.SourceID, e.SampleID = rec.Inputs[0].SourceID, rec.Inputs[0].SampleID
	}
	return e, nil
}

func attrString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// attrFloat reads a number: float64 from JSON, int from records built in Go.
// Absent or non-numeric values read as 0.
func attrFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// unit reads a box coordinate clamped to the frame.
func unit(m map[string]any, key string) float32 {
	return float32(math.Min(1, math.Max(0, attrFloat(m, key))))
}
