package analytics

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"testing"
)

func TestNonFailureOutcomesOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name, event, outcome, class string
		stage, success              bool
		wantOutcome                 string
	}{
		{"start", "run_started", "started", "", true, true, "started"},
		{"progress", "install_progress", "started", "", true, true, "started"},
		{"completed", "run_finished", "completed", "", true, true, "completed"},
		{"cancelled install", "install_finished", "cancelled", "user_cancelled", true, true, "cancelled"},
		{"deferred update", "cli_update_deferred", "cancelled", "user_cancelled", true, true, "cancelled"},
		{"unknown", "run_finished", "unknown", "", true, false, "unknown"},
		{"failed run", "run_finished", "failed", "other", true, false, "failed"},
		{"explicit failure", "run_finished", "failed", "context_canceled", true, false, "failed"},
		{"MCP shutdown", "command_executed", "", "context_canceled", false, true, "cancelled"},
		{"user cancellation", "command_executed", "", "user_cancelled", false, true, "cancelled"},
		{"MCP cancellation result", "command_executed", "", "cancelled", false, true, "cancelled"},
		{"cancelled deploy", "deploy_completed", "", "context_canceled", false, true, "cancelled"},
		{"deadline", "command_executed", "", "context_deadline", false, false, "failed"},
		{"usage error", "command_executed", "", "cli_usage", false, false, "failed"},
		{"device unreachable", "command_executed", "", "device_unreachable", false, false, "failed"},
		{"unknown error", "command_executed", "", "other", false, false, "failed"},
		{"panic", "command_executed", "", "internal_error", false, false, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := make(chan eventPayload, 1)
			enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
				var body eventPayload
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				bodies <- body
				w.WriteHeader(http.StatusNoContent)
			})
			props := map[string]string{"command_name": "wendy run", "success": "false", "error_class": tc.class}
			original := maps.Clone(props)
			if tc.stage {
				TrackStage(context.Background(), tc.event, "wendy run", tc.outcome, tc.class, props)
			} else {
				Track(tc.event, props)
			}
			Close()
			body := <-bodies
			if body.Event != tc.event || body.Success != tc.success || body.ErrorClass != tc.class || body.Properties["outcome"] != tc.wantOutcome {
				t.Fatalf("unexpected wire outcome: %+v", body)
			}
			if !maps.Equal(props, original) {
				t.Fatalf("tracking changed caller properties: %v", props)
			}
		})
	}
}
