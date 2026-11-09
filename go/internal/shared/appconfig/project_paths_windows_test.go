//go:build windows

package appconfig

import (
	"encoding/json"
	"testing"
)

func TestWindowsProjectPathsStayRelative(t *testing.T) {
	for _, tc := range []struct {
		path  string
		valid bool
	}{
		{"camera", true}, {`camera\source`, true}, {"./camera", true}, {".", true},
		{"/camera", false}, {`\camera`, false}, {`C:\camera`, false},
		{`C:camera`, false}, {`\\server\share\camera`, false},
		{`..\camera`, false}, {`camera\..\..\outside`, false}, {"NUL", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := &AppConfig{AppID: "sh.wendy.paths", Services: map[string]*ServiceConfig{"camera": {Context: tc.path}}}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Errorf("service context %q: %v, valid=%v", tc.path, err, tc.valid)
			}
			raw, err := json.Marshal(map[string]any{"components": map[string]any{"camera": map[string]any{"path": tc.path, "tags": []string{"camera"}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseFleetManifest(raw); (err == nil) != tc.valid {
				t.Errorf("fleet path %q: %v, valid=%v", tc.path, err, tc.valid)
			}
		})
	}
}
