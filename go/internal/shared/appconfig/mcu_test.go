package appconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateMCU(t *testing.T) {
	tests := []struct {
		name    string
		mcu     *MCUConfig
		wantErr string
	}{
		{name: "nil", mcu: nil},
		{name: "plain dir", mcu: &MCUConfig{Sketch: "sketch"}},
		{name: "nested dir with profile", mcu: &MCUConfig{Sketch: "apps/mcu", Profile: "default"}},
		{name: "empty sketch", mcu: &MCUConfig{Sketch: ""}, wantErr: "mcu.sketch"},
		{name: "absolute", mcu: &MCUConfig{Sketch: "/abs"}, wantErr: "mcu.sketch"},
		{name: "parent", mcu: &MCUConfig{Sketch: "../x"}, wantErr: "mcu.sketch"},
		{name: "escapes via clean", mcu: &MCUConfig{Sketch: "a/../../x"}, wantErr: "mcu.sketch"},
		{name: "bad profile", mcu: &MCUConfig{Sketch: "sketch", Profile: "bad name"}, wantErr: "mcu.profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMCU(tt.mcu)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMCU() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMCU() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsBadMCU(t *testing.T) {
	cfg := &AppConfig{AppID: "sh.wendy.test", Version: "1.0.0", MCU: &MCUConfig{Sketch: "../x"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mcu.sketch") {
		t.Fatalf("Validate() = %v, want mcu.sketch error", err)
	}
}

func TestArduinoBridgeEntitlementValid(t *testing.T) {
	cfg := &AppConfig{AppID: "sh.wendy.test", Version: "1.0.0",
		Entitlements: []Entitlement{{Type: EntitlementArduinoBridge}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestArduinoBridgeRejectsExtraKeys(t *testing.T) {
	data := []byte(`{"appId":"sh.wendy.test","version":"1.0.0","entitlements":[{"type":"arduino-bridge","path":"x"}]}`)
	warnings := ValidateJSON(data)
	for _, w := range warnings {
		if strings.Contains(w, "path") {
			return
		}
	}
	t.Fatalf("ValidateJSON() warnings = %v, want one naming path", warnings)
}

func TestMCUFieldIsKnown(t *testing.T) {
	data := []byte(`{"appId":"sh.wendy.test","version":"1.0.0","mcu":{"sketch":"sketch"}}`)
	if warnings := ValidateJSON(data); len(warnings) != 0 {
		t.Fatalf("ValidateJSON() warnings = %v, want none", warnings)
	}
}

func TestNoMCUFieldUnchanged(t *testing.T) {
	cfg := AppConfig{AppID: "sh.wendy.test", Version: "1.0.0"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mcu") {
		t.Fatalf("marshaled config without mcu contains it: %s", data)
	}
}

func TestValidateJSONWarnsOnUnknownMCUKeys(t *testing.T) {
	warnings := strings.Join(ValidateJSON([]byte(`{"appId":"a.b","mcu":{"sketch":"sketch","profle":"debug"}}`)), "\n")
	if !strings.Contains(warnings, "Unknown key(s) in mcu") || !strings.Contains(warnings, "profle") {
		t.Fatalf("warnings = %q, want one naming mcu.profle", warnings)
	}
	if w := strings.Join(ValidateJSON([]byte(`{"appId":"a.b","mcu":{"sketch":"sketch","profile":"debug"}}`)), "\n"); strings.Contains(w, "mcu") {
		t.Fatalf("warnings for a valid mcu = %q, want none", w)
	}
}
