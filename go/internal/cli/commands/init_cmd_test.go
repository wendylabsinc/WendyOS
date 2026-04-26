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

func TestResolveInitAppID_RejectsWhitespaceFlag(t *testing.T) {
	_, err := resolveInitAppID("/tmp/demo-app", nil, initOptions{
		appID:    "   ",
		appIDSet: true,
	})
	if err == nil {
		t.Fatal("expected empty --app-id to fail")
	}
	if got := err.Error(); got != "app ID cannot be empty or whitespace" {
		t.Fatalf("error = %q", got)
	}
}

func TestResolveInitAppID_TrimsExplicitFlag(t *testing.T) {
	appID, err := resolveInitAppID("/tmp/demo-app", nil, initOptions{
		appID:    "  demo-app  ",
		appIDSet: true,
	})
	if err != nil {
		t.Fatalf("resolveInitAppID: %v", err)
	}
	if appID != "demo-app" {
		t.Fatalf("appID = %q, want %q", appID, "demo-app")
	}
}

func TestTemplateRunCommand(t *testing.T) {
	tests := []struct {
		name    string
		cwd     string
		destDir string
		appID   string
		want    string
	}{
		{
			name:    "current directory",
			cwd:     "/tmp/demo-app",
			destDir: "/tmp/demo-app",
			appID:   "demo-app",
			want:    "wendy run",
		},
		{
			name:    "new subdirectory",
			cwd:     "/tmp/workspace",
			destDir: "/tmp/workspace/demo-app",
			appID:   "demo-app",
			want:    "cd 'demo-app' && wendy run",
		},
		{
			name:    "new subdirectory with spaces",
			cwd:     "/tmp/workspace",
			destDir: "/tmp/workspace/demo app",
			appID:   "demo app",
			want:    "cd 'demo app' && wendy run",
		},
		{
			name:    "new subdirectory with apostrophe",
			cwd:     "/tmp/workspace",
			destDir: "/tmp/workspace/demo'app",
			appID:   "demo'app",
			want:    "cd 'demo'\"'\"'app' && wendy run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := templateRunCommand(tt.cwd, tt.destDir, tt.appID)
			if got != tt.want {
				t.Fatalf("templateRunCommand(%q, %q, %q) = %q, want %q", tt.cwd, tt.destDir, tt.appID, got, tt.want)
			}
		})
	}
}

func TestBuildInitEntitlementsFromFlags_RejectsEmptyEntitlementFlag(t *testing.T) {
	_, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"", "   "},
	})
	if err == nil {
		t.Fatal("expected empty --entitlement to fail")
	}
	if got := err.Error(); got != "--entitlement requires at least one valid entitlement type" {
		t.Fatalf("error = %q", got)
	}
}

func TestBuildInitEntitlementsFromFlags_IgnoresEmptyEntriesWhenValidEntitlementsExist(t *testing.T) {
	entitlements, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"gpu", "", " usb "},
	})
	if err != nil {
		t.Fatalf("buildInitEntitlementsFromFlags: %v", err)
	}

	gotTypes := map[string]bool{}
	for _, ent := range entitlements {
		gotTypes[ent.Type] = true
	}

	for _, want := range []string{
		appconfig.EntitlementNetwork,
		appconfig.EntitlementGPU,
		appconfig.EntitlementUSB,
	} {
		if !gotTypes[want] {
			t.Fatalf("expected entitlement %q in %+v", want, entitlements)
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
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	// Replace the Bubble Tea checklist with a mock that selects GPU.
	origAsk := askEntitlementQuestions
	askEntitlementQuestions = func(target, language string) ([]appconfig.Entitlement, error) {
		return []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork},
			{Type: appconfig.EntitlementGPU},
		}, nil
	}
	t.Cleanup(func() { askEntitlementQuestions = origAsk })

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

func TestBuildInitEntitlementsFromFlags_Input(t *testing.T) {
	entitlements, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"input"},
	})
	if err != nil {
		t.Fatalf("buildInitEntitlementsFromFlags: %v", err)
	}

	gotTypes := map[string]bool{}
	for _, ent := range entitlements {
		gotTypes[ent.Type] = true
	}

	for _, want := range []string{
		appconfig.EntitlementNetwork,
		appconfig.EntitlementInput,
	} {
		if !gotTypes[want] {
			t.Fatalf("expected entitlement %q in %+v", want, entitlements)
		}
	}
}

func TestBuildInitEntitlementsFromFlags_AllEntitlements(t *testing.T) {
	entitlements, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		allEntitlements: true,
		gpioPinsSet:     true,
		gpioPins:        "17,27",
		i2cDeviceSet:    true,
		i2cDevice:       "/dev/i2c-1",
		persistNameSet:  true,
		persistName:     "test-data",
		persistPathSet:  true,
		persistPath:     "/data",
	})
	if err != nil {
		t.Fatalf("buildInitEntitlementsFromFlags: %v", err)
	}

	gotTypes := map[string]bool{}
	for _, ent := range entitlements {
		gotTypes[ent.Type] = true
	}

	for _, q := range wendyOSEntitlementQuestions {
		if !gotTypes[q.entitlement] {
			t.Errorf("expected entitlement %q from --all-entitlements", q.entitlement)
		}
	}
	if !gotTypes[appconfig.EntitlementNetwork] {
		t.Error("expected network entitlement")
	}
}

func TestBuildInitEntitlementsFromFlags_AllConflictsWithEntitlement(t *testing.T) {
	_, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		allEntitlements: true,
		entitlementsSet: true,
		entitlements:    []string{"gpu"},
	})
	if err == nil {
		t.Fatal("expected error combining --all-entitlements with --entitlement")
	}
}

func TestBuildInitEntitlementsFromFlags_AllMissingFieldFlags(t *testing.T) {
	// --all-entitlements without required field flags for gpio/i2c/persist should error.
	_, err := buildInitEntitlementsFromFlags(targetWendyOS, initOptions{
		allEntitlements: true,
	})
	if err == nil {
		t.Fatal("expected error for --all-entitlements without required field flags")
	}
}

func TestInitCommand_NonInteractiveInput(t *testing.T) {
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
		"--app-id", "scanner-app",
		"--target", "wendyos",
		"--language", "swift",
		"--entitlement", "input",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if !cfg.HasEntitlement(appconfig.EntitlementInput) {
		t.Fatalf("expected input entitlement in %+v", cfg.Entitlements)
	}
}

func TestEntitlementDescriptions_IncludesInput(t *testing.T) {
	desc, ok := entitlementDescriptions[appconfig.EntitlementInput]
	if !ok {
		t.Fatal("entitlementDescriptions missing EntitlementInput entry")
	}
	if desc == "" {
		t.Fatal("entitlementDescriptions[EntitlementInput] is empty")
	}
}

func TestWendyOSEntitlementQuestions_IncludesInput(t *testing.T) {
	found := false
	for _, q := range wendyOSEntitlementQuestions {
		if q.entitlement == appconfig.EntitlementInput {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("wendyOSEntitlementQuestions missing EntitlementInput entry")
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
