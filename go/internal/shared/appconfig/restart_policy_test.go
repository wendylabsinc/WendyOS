package appconfig

import (
	"encoding/json"
	"testing"
)

func TestRestartPolicyConfig(t *testing.T) {
	for _, policy := range []string{"", "no", "on-failure", "unless-stopped", "typo"} {
		data, _ := json.Marshal(map[string]string{"appId": "example", "restartPolicy": policy})
		var cfg AppConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); (err != nil) != (policy == "typo") {
			t.Errorf("policy %q: %v", policy, err)
		}
		if warnings := ValidateJSON(data); len(warnings) != 0 {
			t.Errorf("policy %q: unexpected warnings %v", policy, warnings)
		}
	}
}
