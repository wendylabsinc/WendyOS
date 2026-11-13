package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestTelemetryLogsCompactOverRPC(t *testing.T) {
	var batch agentpb.StreamLogsResponse
	err := protojson.Unmarshal([]byte(`{"logs":{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"camera"}}]},"scopeLogs":[{"scope":{"name":"capture"},"logRecords":[{"timeUnixNano":"1700000000000000001","severityText":"ERROR","body":{"stringValue":"camera busy"},"attributes":[{"key":"frame","value":{"intValue":"9223372036854775807"}}]},{"body":{"stringValue":"retrying"}}]}]}]}}`), &batch)
	if err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeTelemetryServer(t, &fakeTelemetryServer{logBatches: []*agentpb.StreamLogsResponse{&batch}}))
	r, err := s.handleTelemetryLogs(context.Background(), callToolReq("telemetry_logs", map[string]any{"max_records": 1, "max_batches": 1}))
	if err != nil || r.IsError {
		t.Fatalf("logs: %v %v", r, err)
	}
	rows := listPayload(t, r, "logs")
	if len(rows) != 1 {
		t.Fatalf("records: %v", rows)
	}
	row := rows[0]
	if row["body"] != "camera busy" || row["timeUnixNano"] != "1700000000000000001" || row["severityText"] != "ERROR" {
		t.Fatalf("lost log fields: %v", row)
	}
	if row["attributes"].(map[string]any)["frame"] != "9223372036854775807" || row["resource"].(map[string]any)["service.name"] != "camera" || row["scope"].(map[string]any)["name"] != "capture" {
		t.Fatalf("lost precision or identity: %v", row)
	}
	meta := structuredMap(t, r)
	if meta["omitted"] != 1 || meta["returned"] != 1 || meta["collection_limited"] != true {
		t.Fatalf("limits: %v", meta)
	}
}

func TestTelemetryCompactPreservesMetricsAndTraceSemantics(t *testing.T) {
	metrics := json.RawMessage(`{"metrics":{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"requests","unit":"1","sum":{"aggregationTemporality":"AGGREGATION_TEMPORALITY_CUMULATIVE","isMonotonic":true,"dataPoints":[{"asInt":"9223372036854775807","timeUnixNano":"1700000000000000001","attributes":[{"key":"route","value":{"stringValue":"/"}}]}]}},{"name":"latency","histogram":{"aggregationTemporality":"AGGREGATION_TEMPORALITY_DELTA","dataPoints":[{"count":"2","bucketCounts":["1","1"],"explicitBounds":[1],"sum":1.5}]}}]}]}]}}`)
	rows, err := compactTelemetryRows("metrics", []json.RawMessage{metrics})
	if err != nil || len(rows) != 2 {
		t.Fatalf("metrics: %v %v", rows, err)
	}
	if rows[0]["asInt"] != "9223372036854775807" || rows[0]["isMonotonic"] != true || rows[0]["aggregationTemporality"] != "AGGREGATION_TEMPORALITY_CUMULATIVE" || rows[0]["attributes"].(map[string]any)["route"] != "/" {
		t.Fatalf("counter: %v", rows[0])
	}
	if rows[1]["kind"] != "histogram" || rows[1]["count"] != "2" || rows[1]["bucketCounts"].([]any)[1] != "1" {
		t.Fatalf("histogram: %v", rows[1])
	}
	traces := json.RawMessage(`{"traces":{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"request","traceId":"AA==","startTimeUnixNano":"1700000000000000001","endTimeUnixNano":"1700000000000000002","status":{"code":"STATUS_CODE_ERROR","message":"failed"},"events":[{"name":"exception"}]}]}]}]}}`)
	rows, err = compactTelemetryRows("traces", []json.RawMessage{traces})
	if err != nil || len(rows) != 1 || rows[0]["status"].(map[string]any)["code"] != "STATUS_CODE_ERROR" || rows[0]["startTimeUnixNano"] != "1700000000000000001" || len(rows[0]["events"].([]any)) != 1 {
		t.Fatalf("trace: %v %v", rows, err)
	}
}

func TestTelemetryRejectsInvalidBudgetsAndFormats(t *testing.T) {
	for _, args := range []map[string]any{
		{"format": "json"}, {"format": map[string]any{}}, {"max_batches": 1.5}, {"max_batches": 0}, {"max_batches": 101}, {"max_bytes": 0}, {"max_bytes": 1000001}, {"max_records": -1},
	} {
		if _, err := parseTelemetryOptions(callToolReq("telemetry_logs", args)); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestListBudgetRetainsWholeRowsAndCountsOmissions(t *testing.T) {
	items := []map[string]any{}
	for range 20 {
		items = append(items, map[string]any{"body": strings.Repeat("世", 40)})
	}
	r := okRowsBounded("logs", items, map[string]any{"batches_collected": 1}, 512, len(items))
	rows := listPayload(t, r, "logs")
	if len(rows) == 0 || len(rows) >= len(items) || len(toolResultText(t, r)) > 512 {
		t.Fatalf("bad bounded rows: %s", toolResultText(t, r))
	}
	meta := structuredMap(t, r)
	if meta["returned"] != len(rows) || meta["omitted"] != len(items)-len(rows) {
		t.Fatalf("bad omissions: %v", meta)
	}
	if rows[0]["body"] != items[0]["body"] {
		t.Fatal("cut through a row")
	}
	if strings.Contains(toolResultText(t, r), "\n") {
		t.Fatal("JSON fallback should be compact")
	}
}

func TestTelemetryEmptyCompactResult(t *testing.T) {
	opts, _ := parseTelemetryOptions(callToolReq("telemetry_logs", nil))
	for _, kind := range []string{"logs", "metrics", "traces"} {
		r := telemetryResult(kind, nil, opts)
		if r.IsError || len(listPayload(t, r, kind)) != 0 {
			t.Fatalf("empty %s: %v", kind, r)
		}
	}
}
