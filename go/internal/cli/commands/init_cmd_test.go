package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func TestNewInitCmd_Flags(t *testing.T) {
	cmd := newInitCmd()

	expectedFlags := []string{
		"app-id",
		"target",
		"language",
		"entitlement",
		"no-extra-entitlements",
		"gpio-pins",
		"i2c-device",
		"persist-name",
		"persist-path",
		"assistant",
		"install-claude-skills",
	}

	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing init flag %q", name)
		}
	}
}

func TestInitCommand_HelpIncludesExamples(t *testing.T) {
	cmd := newInitCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	expected := []string{
		"Examples:",
		"# Interactive wizard",
		"--persist-name demo-data",
		"--gpio-pins 17,27,22",
		"--no-extra-entitlements",
		"--assistant claude",
		"--install-claude-skills",
	}
	for _, want := range expected {
		if !strings.Contains(output, want) {
			t.Fatalf("expected help output to contain %q, got %q", want, output)
		}
	}
}

func TestInitCommand_NonInteractiveFlagsCreateProject(t *testing.T) {
	tempDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "demo-app",
		"--target", "wendyos",
		"--language", "python",
		"--entitlement", "gpu,usb,persist",
		"--persist-name", "demo-data",
		"--persist-path", "/data",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.AppID != "demo-app" {
		t.Fatalf("AppID = %q, want %q", cfg.AppID, "demo-app")
	}
	if cfg.Platform != appconfig.PlatformWendyOS {
		t.Fatalf("Platform = %q, want %q", cfg.Platform, appconfig.PlatformWendyOS)
	}
	if cfg.Language != "python" {
		t.Fatalf("Language = %q, want %q", cfg.Language, "python")
	}
	if cfg.Python == nil {
		t.Fatal("expected python config to be initialized")
	}

	expectedEntitlements := map[string]bool{
		appconfig.EntitlementNetwork: true,
		appconfig.EntitlementGPU:     true,
		appconfig.EntitlementUSB:     true,
		appconfig.EntitlementPersist: true,
	}
	for _, ent := range cfg.Entitlements {
		delete(expectedEntitlements, ent.Type)
		if ent.Type == appconfig.EntitlementPersist {
			if ent.Name != "demo-data" || ent.Path != "/data" {
				t.Fatalf("persist entitlement = %+v, want name/path populated", ent)
			}
		}
	}
	if len(expectedEntitlements) != 0 {
		t.Fatalf("missing entitlements after init: %v", expectedEntitlements)
	}
}

func TestInitCommand_RejectsPersistWithoutFields(t *testing.T) {
	tempDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "demo-app",
		"--target", "wendyos",
		"--language", "swift",
		"--entitlement", "persist",
		"--assistant", "skip",
	})

	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected missing persist fields to fail")
	}
	if got := err.Error(); got != "persist entitlement requires both --persist-name and --persist-path" {
		t.Fatalf("error = %q", got)
	}
}

func TestInitCommand_NoExtraEntitlementsSkipsPrompts(t *testing.T) {
	tempDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "lite-app",
		"--target", "wendy-lite",
		"--no-extra-entitlements",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.Platform != appconfig.PlatformWendyLite {
		t.Fatalf("Platform = %q, want %q", cfg.Platform, appconfig.PlatformWendyLite)
	}
	if cfg.Language != "swift" {
		t.Fatalf("Language = %q, want %q", cfg.Language, "swift")
	}
	if len(cfg.Entitlements) != 1 || cfg.Entitlements[0].Type != appconfig.EntitlementNetwork {
		t.Fatalf("Entitlements = %+v, want only network", cfg.Entitlements)
	}
}

func TestInitCommand_NoExtraEntitlementsFalseStillPrompts(t *testing.T) {
	tempDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	prevStdin := os.Stdin
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
		os.Stdin = prevStdin
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	inputFile := filepath.Join(tempDir, "stdin.txt")
	if err := os.WriteFile(inputFile, []byte("y\nn\nn\nn\nn\nn\nn\nn\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(inputFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	os.Stdin = f

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "demo-app",
		"--target", "wendyos",
		"--language", "swift",
		"--no-extra-entitlements=false",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if !cfg.HasEntitlement(appconfig.EntitlementGPU) {
		t.Fatalf("expected interactive prompts to run and include %q entitlement, got %+v", appconfig.EntitlementGPU, cfg.Entitlements)
	}
}

func TestInitCommand_InstallClaudeSkillsFalseDoesNotRequireClaude(t *testing.T) {
	tempDir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "lite-app",
		"--target", "wendy-lite",
		"--no-extra-entitlements",
		"--assistant", "skip",
		"--install-claude-skills=false",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
