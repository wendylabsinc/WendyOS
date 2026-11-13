package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestActivationAttemptsAndPrivacy(t *testing.T) {
	first := NewAttempt(context.Background())
	if AttemptID(first) != AttemptID(WithAttempt(first)) {
		t.Fatal("nested operation lost its attempt")
	}
	if AttemptID(first) == AttemptID(NewAttempt(context.Background())) {
		t.Fatal("independent MCP operations share an attempt")
	}
	for _, tc := range []struct {
		key, value string
		want       bool
	}{
		{"step", "download", true}, {"step", "/Users/alice/token", false},
		{"deploy_build_ms", "100", true}, {"deploy_build_ms", "-1", false},
		{"error_detail", "credentials", false}, {"deploy_device_type", "private-host", false},
		{"outcome", "cancelled", true}, {"auth_valid", "unknown", true},
		{"deploy_device_type", "vm-arm64", true},
	} {
		if got := allowedProperty(tc.key, tc.value); got != tc.want {
			t.Errorf("allowedProperty(%q,%q)=%v", tc.key, tc.value, got)
		}
	}
}

func TestActivationStageSuppliesCannedDiagnostics(t *testing.T) {
	var properties map[string]string
	SetTrackHookForTesting(func(_ string, props map[string]string) { properties = props })
	t.Cleanup(func() { SetTrackHookForTesting(nil) })
	TrackStage(context.Background(), "journey_completed", "wendy tour", "failed", "tool_not_found", nil)
	code, detail := Diagnostic("tool_not_found")
	if properties["error_code"] != code || properties["error_detail"] != detail {
		t.Fatalf("missing stable diagnostics: %v", properties)
	}
}

// The receiver integration suite consumes this actual producer payload when
// CLI_TELEMETRY_CONTRACT_PATH is set to the same local path in both test runs.
func TestActivationProducerContract(t *testing.T) {
	bodies := make(chan []byte, 1)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var payload eventPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Error(err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := NewAttempt(context.Background())
	TrackStage(ctx, "app_started", "wendy run", "completed", "", map[string]string{"deploy_transport": "chunk", "deploy_build_ms": "42", "private_path": "/Users/alice/private"})
	Close()
	body := <-bodies
	var payload eventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(payload.EventID); err != nil {
		t.Fatal(err)
	}
	if payload.AttemptID != AttemptID(ctx) || payload.SchemaVersion != 2 || !payload.Success {
		t.Fatalf("invalid envelope: %s", body)
	}
	if time.Since(payload.OccurredAt) > time.Minute {
		t.Fatal("event occurrence time missing")
	}
	if len(payload.Properties) != 4 {
		t.Fatalf("unexpected properties: %v", payload.Properties)
	}
	if path := os.Getenv("CLI_TELEMETRY_CONTRACT_PATH"); path != "" {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
