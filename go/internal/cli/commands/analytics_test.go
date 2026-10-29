package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestAnalyticsCommand_HasSubcommands(t *testing.T) {
	cmd := newAnalyticsCmd()

	expectedSubcmds := []string{"enable", "disable", "status"}
	cmds := cmd.Commands()
	cmdNames := make(map[string]bool)
	for _, c := range cmds {
		cmdNames[c.Name()] = true
	}

	for _, name := range expectedSubcmds {
		if !cmdNames[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestAnalyticsCommand_StatusOutput(t *testing.T) {
	root := NewRootCmd()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"analytics", "status"})

	// Set env to get predictable output and avoid writing to real home.
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

	err := root.Execute()
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "disabled") {
		t.Errorf("expected 'disabled' in output, got: %q", output)
	}
}

func isolateAnalyticsPreference(t *testing.T, cfg *config.Config) string {
	t.Helper()
	for _, key := range env.CIEnvVars {
		t.Setenv(key, "")
	}
	t.Setenv("WENDY_ANALYTICS", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	analytics.Disable()
	t.Cleanup(analytics.Disable)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAnalyticsCommandStatusReportsEffectiveState(t *testing.T) {
	for _, tc := range []struct {
		name            string
		preference      *config.AnalyticsConfig
		ci              bool
		envOverride     bool
		identityFailure bool
		want            string
	}{
		{name: "default enabled", want: "Analytics: enabled"},
		{name: "saved enabled", preference: &config.AnalyticsConfig{Enabled: true}, want: "Analytics: enabled"},
		{name: "saved disabled", preference: &config.AnalyticsConfig{Enabled: false}, want: "Analytics: disabled"},
		{name: "environment override", preference: &config.AnalyticsConfig{Enabled: true}, envOverride: true, want: "Analytics: disabled (overridden by WENDY_ANALYTICS=false)"},
		{name: "CI overrides enabled preference", preference: &config.AnalyticsConfig{Enabled: true}, ci: true, want: "Analytics: disabled (CI environment detected)"},
		{name: "CI takes precedence over environment override", preference: &config.AnalyticsConfig{Enabled: true}, ci: true, envOverride: true, want: "Analytics: disabled (CI environment detected)"},
		{name: "identity initialization failed", preference: &config.AnalyticsConfig{Enabled: true}, identityFailure: true, want: "Analytics: disabled (analytics initialization failed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Analytics: tc.preference}
			dir := isolateAnalyticsPreference(t, cfg)
			if tc.ci {
				t.Setenv("GITHUB_ACTIONS", "true")
			}
			if tc.envOverride {
				t.Setenv("WENDY_ANALYTICS", "false")
			}
			if tc.identityFailure {
				// A directory at the file path fails even when tests run as root.
				if err := os.Mkdir(filepath.Join(dir, "analytics_id"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			analytics.Init(cfg)
			var output bytes.Buffer
			cmd := newAnalyticsStatusCmd()
			cmd.SetArgs([]string{})
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(output.String()); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnalyticsEnableAppliesPreferenceToCurrentInvocation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		ci              bool
		envOverride     bool
		identityFailure bool
		wantEnabled     bool
	}{
		{name: "enabled immediately", wantEnabled: true},
		{name: "CI remains disabled", ci: true},
		{name: "environment override remains disabled", envOverride: true},
		{name: "identity failure remains disabled", identityFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Analytics: &config.AnalyticsConfig{Enabled: false}}
			dir := isolateAnalyticsPreference(t, cfg)
			if tc.ci {
				t.Setenv("CI", "true")
				t.Setenv("WENDY_ANALYTICS", "true")
			}
			if tc.envOverride {
				t.Setenv("WENDY_ANALYTICS", "false")
			}
			if tc.identityFailure {
				if err := os.Mkdir(filepath.Join(dir, "analytics_id"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			analytics.Init(cfg)
			if analytics.Enabled() {
				t.Fatal("test must begin with analytics disabled")
			}
			var output bytes.Buffer
			cmd := newAnalyticsEnableCmd()
			cmd.SetArgs([]string{})
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := analytics.Enabled(); got != tc.wantEnabled {
				t.Fatalf("effective analytics enabled = %v, want %v", got, tc.wantEnabled)
			}
			saved, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Analytics == nil || !saved.Analytics.Enabled {
				t.Fatal("enable did not persist the requested preference")
			}
			if tc.ci || tc.envOverride {
				if _, err := os.Stat(filepath.Join(dir, "analytics_id")); !os.IsNotExist(err) {
					t.Fatalf("disabled invocation created analytics identity: %v", err)
				}
			}
		})
	}
}
