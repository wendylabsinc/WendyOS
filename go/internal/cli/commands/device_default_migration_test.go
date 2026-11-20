package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRemovedDeviceDefaultCommandsExplainMigrationWithoutChangingConfig(t *testing.T) {
	for _, name := range []string{"set-default", "get-default", "unset-default"} {
		t.Run(name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			dir := t.TempDir()
			t.Setenv("WENDY_CONFIG_DIR", dir)
			path := filepath.Join(dir, "config.json")
			original := []byte(`{"defaultDevice":"old.local","devicePins":{"old":{"assetId":"42"}}}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			root := NewRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			args := []string{"device", name}
			if name == "set-default" {
				args = append(args, "new.local")
			}
			root.SetArgs(args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "was removed; use --device <device> or set WENDY_DEVICE") {
				t.Fatalf("expected migration error, got %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("unexpected stdout: %s", &output)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("migration command changed config")
			}
		})
	}
}

func TestLegacyDefaultNoticeUsesStderrAndRespectsExplicitTarget(t *testing.T) {
	restoreDeviceGlobals(t)
	for _, target := range []string{"", "chosen.local"} {
		deviceFlag = target
		root := NewRootCmd()
		// NewRootCmd registers the flag and resets its value.
		deviceFlag = target
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		cfg := &config.Config{LegacyDefaultDevice: "old.local"}
		noteLegacyDefaultDevice(root, cfg)
		if stdout.Len() != 0 {
			t.Fatal("migration notice contaminated stdout")
		}
		if target == "" && !strings.Contains(stderr.String(), "saved default device is ignored") {
			t.Fatalf("missing notice: %q", stderr.String())
		}
		if target != "" && stderr.Len() != 0 {
			t.Fatalf("unnecessary notice with explicit target: %q", stderr.String())
		}
	}
}
