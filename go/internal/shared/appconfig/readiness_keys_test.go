package appconfig

import (
	"strings"
	"testing"
)

func TestWarnUnknownReadinessKeysAtEveryScope(t *testing.T) {
	warnings := ValidateJSON([]byte(`{"appId":"app","readiness":{"initialDelaySeconds":10},"services":{"api":{"context":".","readiness":{"timeout":10,"tcpSocket":{"port":8080,"host":"wrong"}}}}}`))
	text := strings.Join(warnings, "\n")
	for _, key := range []string{"readiness", "initialDelaySeconds", `services["api"].readiness`, "timeout", "host"} {
		if !strings.Contains(text, key) {
			t.Fatalf("missing %q in %s", key, text)
		}
	}
}
