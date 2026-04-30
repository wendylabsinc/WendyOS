package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceAppsListCommand_HelpDescribesDeployedApps(t *testing.T) {
	cmd := newDeviceCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"apps", "list", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "List deployed applications") {
		t.Fatalf("expected help output to contain %q, got %q", "List deployed applications", output)
	}
	if strings.Contains(output, "List running applications") {
		t.Fatalf("expected help output to avoid stale wording, got %q", output)
	}
}

func TestLoadAppConfigForStart(t *testing.T) {
	dir := t.TempDir()

	write := func(t *testing.T, name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	t.Run("no wendy.json and no --config returns nil", func(t *testing.T) {
		cfg, err := loadAppConfigForStart(dir, "", "anything")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil cfg, got %+v", cfg)
		}
	})

	write(t, "wendy.json", `{"appId":"my-app","version":"0.1.0"}`)

	t.Run("wendy.json in cwd with matching appId is loaded", func(t *testing.T) {
		cfg, err := loadAppConfigForStart(dir, "", "my-app")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cfg == nil || cfg.AppID != "my-app" {
			t.Fatalf("expected cfg.AppID=my-app, got %+v", cfg)
		}
	})

	t.Run("wendy.json in cwd with mismatched appId returns nil (with notice)", func(t *testing.T) {
		cfg, err := loadAppConfigForStart(dir, "", "other-app")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil cfg on mismatch, got %+v", cfg)
		}
	})

	other := write(t, "other.json", `{"appId":"other-app","version":"0.1.0"}`)

	t.Run("--config with matching appId is loaded", func(t *testing.T) {
		cfg, err := loadAppConfigForStart(dir, other, "other-app")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cfg == nil || cfg.AppID != "other-app" {
			t.Fatalf("expected cfg.AppID=other-app, got %+v", cfg)
		}
	})

	t.Run("--config with mismatched appId returns error", func(t *testing.T) {
		_, err := loadAppConfigForStart(dir, other, "my-app")
		if err == nil {
			t.Fatalf("expected error on appId mismatch")
		}
	})

	t.Run("--config pointing at missing file returns error", func(t *testing.T) {
		_, err := loadAppConfigForStart(dir, filepath.Join(dir, "nope.json"), "my-app")
		if err == nil {
			t.Fatalf("expected error for missing file")
		}
	})

	t.Run("malformed wendy.json in cwd returns nil (with notice)", func(t *testing.T) {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, "wendy.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write bad json: %v", err)
		}
		cfg, err := loadAppConfigForStart(bad, "", "anything")
		if err != nil {
			t.Fatalf("expected nil error for malformed implicit config, got %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil cfg for malformed implicit config, got %+v", cfg)
		}
	})
}
