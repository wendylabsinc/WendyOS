package analytics

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func TestRecoveryTelemetryCarriesSafeOutcomeAndDiagnostic(t *testing.T) {
	old := version.Version
	version.Version = "dev-battery-watch"
	t.Cleanup(func() { version.Version = old })
	bodies := make(chan eventPayload, 3)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var p eventPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		bodies <- p
	})
	for _, class := range []string{"build_failed", "context_canceled", ""} {
		success := "false"
		if class == "" {
			success = "true"
		}
		Track("command_executed", map[string]string{"command_name": "wendy run", "success": success, "error_class": class, "error_detail": "secret/path/credential"})
	}
	Close()
	for range 3 {
		p := <-bodies
		if !p.IsDevBuild {
			t.Fatal("custom development build reported as release")
		}
		encoded, _ := json.Marshal(p)
		if strings.Contains(string(encoded), "secret/") {
			t.Fatal("raw error detail escaped")
		}
		switch p.ErrorClass {
		case "build_failed":
			if p.Properties["outcome"] != "failed" || p.Properties["error_code"] != "build_failed" || p.ErrorDetail == "" || p.ErrorDetail != p.Properties["error_detail"] {
				t.Fatalf("missing failure context: %+v", p)
			}
		case "context_canceled":
			if p.Properties["outcome"] != "cancelled" || p.ErrorDetail != "" {
				t.Fatalf("cancellation is a failure: %+v", p)
			}
		default:
			if p.Properties["outcome"] != "completed" || p.ErrorDetail != "" {
				t.Fatalf("missing completion: %+v", p)
			}
		}
	}
}
