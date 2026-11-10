package backend

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

var trendsTool = dataChatTool{Tool: chat.Tool{Name: "device_data_compare", Description: "Compare retained event occurrence counts in two explicit time windows, grouped by event, app, or source. Uses one journal snapshot for both windows. Counts are event records, not unique people, boxes, or total activity. Full-history comparison and causal claims are not supported by this bounded journal. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"campaign":{"type":"string"},"app_id":{"type":"string"},"event":{"type":"string"},"source_id":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"compare_start":{"type":"string"},"compare_end":{"type":"string"},"group_by":{"type":"string","enum":["event","app","source"]}},"required":["start","end","compare_start","compare_end","group_by"],"additionalProperties":false}`)}, run: compareChatEvents}

func compareChatEvents(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Campaign     string `json:"campaign"`
		AppID        string `json:"app_id"`
		Event        string `json:"event"`
		SourceID     string `json:"source_id"`
		Start        string `json:"start"`
		End          string `json:"end"`
		CompareStart string `json:"compare_start"`
		CompareEnd   string `json:"compare_end"`
		Group        string `json:"group_by"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Start == "" || a.End == "" || a.CompareStart == "" || a.CompareEnd == "" {
		return nil, errors.New("provide explicit bounds for both comparison windows")
	}
	if a.Group != "event" && a.Group != "app" && a.Group != "source" {
		return nil, errors.New("group_by must be event, app, or source")
	}
	first, err := parseDataWindow(a.Start, a.End)
	if err != nil {
		return nil, err
	}
	second, err := parseDataWindow(a.CompareStart, a.CompareEnd)
	if err != nil {
		return nil, err
	}
	query := dataEventArgs{Campaign: a.Campaign, AppID: a.AppID, Event: a.Event, SourceID: a.SourceID}
	retained, err := t.retainedEvents(ctx, query)
	if err != nil {
		return nil, err
	}
	left, unknown := filterChatEvents(retained.events, query, first)
	right, _ := filterChatEvents(retained.events, query, second)
	l, r := countEventGroups(left, a.Group), countEventGroups(right, a.Group)
	keys := map[string]bool{}
	for k := range l {
		keys[k] = true
	}
	for k := range r {
		keys[k] = true
	}
	sorted := []string{}
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	rows := []any{}
	for _, k := range sorted {
		rows = append(rows, map[string]any{"group": k, "first_count": l[k], "second_count": r[k], "second_minus_first": r[k] - l[k]})
	}
	return map[string]any{"first_window": first.evidence(), "second_window": second.evidence(), "first_duration_seconds": first.end.Sub(first.start).Seconds(), "second_duration_seconds": second.end.Sub(second.start).Seconds(), "first_retained_count": len(left), "second_retained_count": len(right), "groups": rows, "journal_gap": retained.gap, "journal_cursor": retained.cursor, "invalid_timestamp_count": unknown, "history_complete": false, "unit": "event occurrences, not unique objects", "coverage": "Both windows use one retained journal, at most 512 entries. A difference is only a difference among retained events, not evidence that actual activity rose or fell. Sources may overlap; group totals need not equal event count."}, nil
}
func countEventGroups(events []data.DeviceEvent, group string) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		keys := map[string]bool{}
		switch group {
		case "event":
			keys[e.Name] = true
		case "app":
			keys[e.AppID] = true
		case "source":
			if source, ok := e.Attributes["source_id"].(string); ok && source != "" {
				keys[source] = true
			}
			for _, input := range e.Inputs {
				if input.SourceID != "" {
					keys[input.SourceID] = true
				}
			}
		}
		if len(keys) == 0 {
			keys["unknown"] = true
		}
		for k := range keys {
			if k == "" {
				k = "unknown"
			}
			out[k]++
		}
	}
	return out
}
