package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestAppInspectFieldSelectionSkipsUnrequestedDiagnostics(t *testing.T) {
	agent := &appInspectAgent{
		container:  &agentpb.AppContainer{AppName: "robot", RunningState: agentpb.AppRunningState_RUNNING},
		logRequest: make(chan *agentpb.StreamLogsRequest, 1), holdLogs: true,
	}
	s := New(&config.Config{}, nil)
	s.SetConn(appInspectConnection(t, agent))
	r, err := s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{
		"app_name": "robot", "fields": []string{"state", "readiness"},
	}))
	if err != nil || r.IsError {
		t.Fatalf("inspection: %v %v", r, err)
	}
	out := structuredMap(t, r)
	if out["state"] == nil || out["readiness"] == nil || out["usage"] != nil || out["recent_logs"] != nil {
		t.Fatalf("unrequested sections returned: %+v", out)
	}
	select {
	case <-agent.logRequest:
		t.Fatal("unrequested log stream was opened")
	default:
	}
	if out["readiness"].(map[string]any)["status"] != "unknown" {
		t.Fatal("missing readiness declaration must stay unknown")
	}
}

func TestAppInspectRejectsInvalidFieldSelection(t *testing.T) {
	for _, value := range []any{nil, "state", []any{1}, []string{"secret"}} {
		_, err := appInspectFields(callToolReq("app_inspect", map[string]any{"fields": value}))
		if err == nil {
			t.Fatalf("accepted fields: %#v", value)
		}
	}
	fields, err := appInspectFields(callToolReq("app_inspect", map[string]any{"fields": []string{}}))
	if err != nil || len(fields) != 1 || !fields["state"] {
		t.Fatalf("empty selection must retain state: %v %v", fields, err)
	}
}

func TestAppInspectCollapsesRepeatedLogsWithoutLosingProvenance(t *testing.T) {
	first := map[string]any{"body": "missing rollout", "timeUnixNano": "10", "traceId": "a", "resource": map[string]any{"service.name": "robot"}}
	second := map[string]any{"body": "missing rollout", "timeUnixNano": "20", "traceId": "a", "resource": map[string]any{"service.name": "robot"}}
	otherTrace := map[string]any{"body": "missing rollout", "timeUnixNano": "30", "traceId": "b", "resource": map[string]any{"service.name": "robot"}}
	rows := appendAppInspectLog(nil, first)
	rows = appendAppInspectLog(rows, second)
	if len(rows) != 1 || rows[0]["repeat_count"] != 2 || rows[0]["first_time_unix_nano"] != "10" || rows[0]["last_time_unix_nano"] != "20" {
		t.Fatalf("repeat evidence lost: %+v", rows)
	}
	rows = appendAppInspectLog(rows, otherTrace)
	if len(rows) != 2 {
		t.Fatal("different trace identities must not be merged")
	}
}

func TestAppInspectRepeatedDiagnosticPayloadIsBounded(t *testing.T) {
	var rows []map[string]any
	for i := 0; i < 100; i++ {
		rows = appendAppInspectLog(rows, map[string]any{"body": strings.Repeat("missing rollout ", 50), "timeUnixNano": i})
	}
	encoded, _ := json.Marshal(rows)
	if len(rows) != 1 || rows[0]["repeat_count"] != 100 || len(encoded) > 1200 {
		t.Fatalf("repeated log payload grew: %d rows / %d bytes", len(rows), len(encoded))
	}
	if appInspectLogCount(rows[0]) != 100 {
		t.Fatal("omission accounting must count original events, not grouped rows")
	}
}
