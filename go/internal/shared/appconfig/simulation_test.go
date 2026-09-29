package appconfig

import (
	"encoding/json"
	"testing"
)

func TestSimulationServiceConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, sim string
		valid     bool
	}{
		{"valid", `{"profile":"rosmaster-r2","entitlements":[{"type":"network","mode":"host"}],"env":{"R2_SIMULATOR_URL":"http://127.0.0.1:8890"}}`, true},
		{"unknown profile", `{"profile":"unknown","entitlements":[{"type":"network","mode":"host"}]}`, false},
		{"implicit grants", `{"profile":"rosmaster-r2"}`, false},
		{"bad environment", `{"profile":"rosmaster-r2","entitlements":[{"type":"network","mode":"host"}],"env":{"INVALID-KEY":"1"}}`, false},
		{"bad grants", `{"profile":"rosmaster-r2","entitlements":[{"type":"unknown"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"appId":"test","services":{"camera":{"context":"camera","simulation":` + tc.sim + `}}}`)
			var cfg AppConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
			if tc.valid {
				if warnings := ValidateJSON(data); len(warnings) != 0 {
					t.Fatalf("schema warnings: %v", warnings)
				}
			}
		})
	}
}
