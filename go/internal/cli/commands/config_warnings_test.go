package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAppConfigWarningsFromFile_DeprecatedVideoEntitlement(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "wendy.json")
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "video"}
		]
	}`)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	warnings, err := appConfigWarningsFromFile(cfgPath)
	if err != nil {
		t.Fatalf("appConfigWarningsFromFile() error = %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("appConfigWarningsFromFile() got %d warnings, want 1", len(warnings))
	}
	if got := warnings[0]; got != `entitlement[0]: "video" is deprecated; use "camera" instead` {
		t.Fatalf("appConfigWarningsFromFile() warning = %q", got)
	}
}

func TestPrintAppConfigWarnings(t *testing.T) {
	var buf bytes.Buffer

	printAppConfigWarnings(&buf, []string{"first warning", "second warning"})

	if got := buf.String(); got != "Warning: first warning\nWarning: second warning\n" {
		t.Fatalf("printAppConfigWarnings() output = %q", got)
	}
}
