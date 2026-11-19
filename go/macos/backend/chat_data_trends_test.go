package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"testing"
)

func TestChatDataCompareCountsOccurrencesNotBoxes(t *testing.T) {
	c := &chatDataClient{gap: true, events: []data.DeviceEvent{
		{ID: "a", ObservedAt: "2026-11-06T10:00:00Z", Attributes: map[string]any{"source_id": "front", "detections": []any{"person", "person"}}, Inputs: []data.SampleRef{{SourceID: "front"}, {SourceID: "front"}}},
		{ID: "b", ObservedAt: "2026-11-07T10:00:00Z", Attributes: map[string]any{"source_id": "front"}},
		{ID: "c", ObservedAt: "2026-11-07T11:00:00Z", Attributes: map[string]any{"source_id": "front"}},
	}}
	out := runDataTest(t, c, "device_data_compare", `{"start":"2026-11-06T00:00:00Z","end":"2026-11-07T00:00:00Z","compare_start":"2026-11-07T00:00:00Z","compare_end":"2026-11-08T00:00:00Z","group_by":"source"}`)
	rows := out["groups"].([]any)
	if len(rows) != 1 {
		t.Fatal(out)
	}
	r := rows[0].(map[string]any)
	if r["first_count"] != float64(1) || r["second_count"] != float64(2) || r["second_minus_first"] != float64(1) || out["history_complete"] != false {
		t.Fatal(out)
	}
}
