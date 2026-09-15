package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestNewInitCmd_Flags(t *testing.T) {
	cmd := newInitCmd()

	expectedFlags := []string{
		"app-id",
		"here",
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
		"framework",
		"ros2-domain-id",
		"ros2-rmw",
		"ros2-distro",
		"ros2-discovery-scope",
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
		// WDY frameworks discoverability: `wendy init --help` must at least
		// mention that a "frameworks" key exists and how to reach it.
		"frameworks",
		"--framework ros2",
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

// stubInitDestPrompts replaces the TTY detection and the destination/name
// prompts used by resolveInitDestAndID with canned answers.
func stubInitDestPrompts(t *testing.T, interactive, useCurrentDir bool, projectName string) {
	t.Helper()
	origInteractive := isInteractiveTerminalFn
	origConfirm := confirmInitCurrentDir
	origPrompt := promptInitProjectName
	isInteractiveTerminalFn = func() bool { return interactive }
	confirmInitCurrentDir = func() (bool, error) { return useCurrentDir, nil }
	promptInitProjectName = func() (string, error) { return projectName, nil }
	t.Cleanup(func() {
		isInteractiveTerminalFn = origInteractive
		confirmInitCurrentDir = origConfirm
		promptInitProjectName = origPrompt
	})
}

func TestResolveInitDestAndID_ExplicitAppIDCreatesSubdirectory(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/workspace", nil, initOptions{
		appID:    "demo-app",
		appIDSet: true,
	})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != filepath.Join("/tmp/workspace", "demo-app") || appID != "demo-app" {
		t.Fatalf("destDir, appID = %q, %q, want subdirectory demo-app", destDir, appID)
	}
}

// The WDY-1805 repro: partial flags like --target must not suppress the
// destination and name questions on a TTY.
func TestResolveInitDestAndID_PromptsDespiteDirectiveFlags(t *testing.T) {
	stubInitDestPrompts(t, true, false, "demo-app")

	destDir, appID, err := resolveInitDestAndID("/tmp/Build", nil, initOptions{
		targetSet:   true,
		languageSet: true,
		templateSet: true,
	})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != filepath.Join("/tmp/Build", "demo-app") || appID != "demo-app" {
		t.Fatalf("destDir, appID = %q, %q, want prompted demo-app subdirectory", destDir, appID)
	}
}

func TestResolveInitDestAndID_CurrentDirUsesValidatedBasename(t *testing.T) {
	stubInitDestPrompts(t, true, true, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/demo-app", nil, initOptions{targetSet: true})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != "/tmp/demo-app" || appID != "demo-app" {
		t.Fatalf("destDir, appID = %q, %q, want current dir demo-app", destDir, appID)
	}
}

func TestResolveInitDestAndID_CurrentDirRejectsInvalidBasename(t *testing.T) {
	stubInitDestPrompts(t, true, true, "")

	_, _, err := resolveInitDestAndID("/tmp/Demo App", nil, initOptions{})
	if err == nil {
		t.Fatal("expected invalid directory basename to fail as app ID")
	}
	if !strings.Contains(err.Error(), `"Demo App"`) || !strings.Contains(err.Error(), "--app-id") {
		t.Fatalf("error = %q, want mention of basename and --app-id", err)
	}
}

func TestResolveInitDestAndID_NonInteractiveWithoutAppIDFails(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	_, _, err := resolveInitDestAndID("/tmp/Build", nil, initOptions{
		targetSet:   true,
		languageSet: true,
		templateSet: true,
	})
	if err == nil {
		t.Fatal("expected non-interactive template init without app ID to fail")
	}
	if !strings.Contains(err.Error(), "--app-id") {
		t.Fatalf("error = %q, want mention of --app-id", err)
	}
}

// WDY-2439: `wendy init --here` scaffolds into the current directory instead
// of nesting a subdirectory named after an explicit app ID (the reporter's
// `wendy init cctv-demo` inside an existing empty `cctv-demo/` repro).

func TestResolveInitDestAndID_HereWithExplicitAppIDUsesCwd(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/cctv-demo", nil, initOptions{
		here:     true,
		appID:    "cctv-demo",
		appIDSet: true,
	})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != "/tmp/cctv-demo" || appID != "cctv-demo" {
		t.Fatalf("destDir, appID = %q, %q, want (%q, %q)", destDir, appID, "/tmp/cctv-demo", "cctv-demo")
	}
}

func TestResolveInitDestAndID_HereWithPositionalAppIDUsesCwd(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/cctv-demo", []string{"cctv-demo"}, initOptions{
		here: true,
	})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != "/tmp/cctv-demo" || appID != "cctv-demo" {
		t.Fatalf("destDir, appID = %q, %q, want (%q, %q)", destDir, appID, "/tmp/cctv-demo", "cctv-demo")
	}
}

func TestResolveInitDestAndID_HereWithoutNameUsesValidatedBasename(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/demo-app", nil, initOptions{here: true})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != "/tmp/demo-app" || appID != "demo-app" {
		t.Fatalf("destDir, appID = %q, %q, want (%q, %q)", destDir, appID, "/tmp/demo-app", "demo-app")
	}
}

func TestResolveInitDestAndID_HereWithoutNameRejectsInvalidBasename(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	_, _, err := resolveInitDestAndID("/tmp/Demo App", nil, initOptions{here: true})
	if err == nil {
		t.Fatal("expected invalid directory basename to fail as app ID")
	}
	if !strings.Contains(err.Error(), `"Demo App"`) || !strings.Contains(err.Error(), "wendy init --here <name>") {
		t.Fatalf("error = %q, want mention of basename and %q", err, "wendy init --here <name>")
	}
}

// --here must work with no TTY attached at all: today the no-name path
// hard-errors without an app ID when isInteractiveTerminalFn is false, but
// --here answers the destination/name question itself and must never reach
// that check.
func TestResolveInitDestAndID_HereWorksNonInteractively(t *testing.T) {
	stubInitDestPrompts(t, false, false, "")

	destDir, appID, err := resolveInitDestAndID("/tmp/demo-app", nil, initOptions{
		here:        true,
		targetSet:   true,
		languageSet: true,
		templateSet: true,
	})
	if err != nil {
		t.Fatalf("resolveInitDestAndID: %v", err)
	}
	if destDir != "/tmp/demo-app" || appID != "demo-app" {
		t.Fatalf("destDir, appID = %q, %q, want (%q, %q)", destDir, appID, "/tmp/demo-app", "demo-app")
	}
}

func TestPathHasPrefix_IsCaseSensitiveOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths are intentionally compared case-insensitively")
	}
	if pathHasPrefix("/tmp/Foo/app", "/tmp/foo") {
		t.Fatal("pathHasPrefix should not case-fold Unix paths")
	}
}

func TestValidateNewProjectName_RejectsNonSubdirectoryNames(t *testing.T) {
	for _, value := range []string{"", "   ", ".", "..", "../outside", "nested/app", `nested\app`, "/tmp/app", "C:app", "demo app", "demo'app", "-demo", ".demo"} {
		t.Run(value, func(t *testing.T) {
			if err := validateNewProjectName(value); err == nil {
				t.Fatalf("validateNewProjectName(%q) = nil, want error", value)
			}
		})
	}
}

func TestValidateNewProjectName_AcceptsPlainDirectoryNames(t *testing.T) {
	for _, value := range []string{"demo-app", "demo.app", "demo_app"} {
		t.Run(value, func(t *testing.T) {
			if err := validateNewProjectName(value); err != nil {
				t.Fatalf("validateNewProjectName(%q): %v", value, err)
			}
		})
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

func TestTemplateNextSteps(t *testing.T) {
	tests := []struct {
		name    string
		cwd     string
		destDir string
		appID   string
		want    []string
	}{
		{
			name:    "current directory",
			cwd:     "/tmp/demo-app",
			destDir: "/tmp/demo-app",
			appID:   "demo-app",
			want:    []string{"wendy run"},
		},
		{
			name:    "new subdirectory",
			cwd:     "/tmp/workspace",
			destDir: "/tmp/workspace/demo-app",
			appID:   "demo-app",
			want:    []string{"cd 'demo-app'", "wendy run"},
		},
		{
			name:    "new subdirectory with apostrophe",
			cwd:     "/tmp/workspace",
			destDir: "/tmp/workspace/demo'app",
			appID:   "demo'app",
			want:    []string{"cd 'demo'\"'\"'app'", "wendy run"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := templateNextSteps(tt.cwd, tt.destDir, tt.appID)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("templateNextSteps(%q, %q, %q) = %#v, want %#v", tt.cwd, tt.destDir, tt.appID, got, tt.want)
			}
		})
	}
}

// WDY-2439: the template flow must refuse to scaffold into a directory that
// already has a wendy.json when dest == cwd (the --here case), and must fail
// before any filesystem mutation (MkdirAll, template download/render).
func TestRunTemplateFlow_HereGuardsExistingWendyJSON(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "wendy.json"), []byte(`{"appId":"existing"}`), 0o644); err != nil {
		t.Fatalf("writing existing wendy.json: %v", err)
	}

	err := runTemplateFlow(tempDir, tempDir, "demo-app", "simple-api", targetWendyOS, &repoMeta{}, initOptions{here: true})
	if err == nil {
		t.Fatal("expected runTemplateFlow to fail when wendy.json already exists in the current directory")
	}
	if got, want := err.Error(), "wendy.json already exists here; run from an empty directory or remove it first"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestResolveTemplateLanguage_RejectsUnavailableTemplateLanguage(t *testing.T) {
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "realsense-camera", Languages: []string{langPython}},
		},
		Languages: []repoMetaLanguage{
			{Key: langPython, Name: "Python"},
			{Key: langSwift, Name: "Swift"},
		},
	}

	_, err := resolveTemplateLanguage(targetWendyOS, "realsense-camera", meta, initOptions{
		language:    langSwift,
		languageSet: true,
	})
	if err == nil {
		t.Fatal("expected unavailable template language to fail")
	}
	if got, want := err.Error(), `template "realsense-camera" is not available for language "swift" (available: python)`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestTemplateTargetMatch_DefaultsToWendyOSOnly(t *testing.T) {
	if !templateTargetMatch(repoMetaTemplate{Name: "simple-api"}, targetWendyOS) {
		t.Fatal("template without targets should match WendyOS")
	}
	if templateTargetMatch(repoMetaTemplate{Name: "simple-api"}, targetDarwin) {
		t.Fatal("template without targets should not match Darwin")
	}
}

func TestTemplateTargetMatch_AcceptsExplicitDarwinTarget(t *testing.T) {
	tmpl := repoMetaTemplate{Name: "mac-llm", Targets: []string{targetDarwin}}
	if !templateTargetMatch(tmpl, targetDarwin) {
		t.Fatal("template with darwin target should match Darwin")
	}
	if templateTargetMatch(tmpl, targetWendyOS) {
		t.Fatal("template with only darwin target should not match WendyOS")
	}
}

func TestResolveTemplateLanguage_DarwinRequiresSwift(t *testing.T) {
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "mac-llm", Languages: []string{langSwift}, Targets: []string{targetDarwin}},
		},
		Languages: []repoMetaLanguage{
			{Key: langPython, Name: "Python"},
			{Key: langSwift, Name: "Swift"},
		},
	}

	language, err := resolveTemplateLanguage(targetDarwin, "mac-llm", meta, initOptions{})
	if err != nil {
		t.Fatalf("resolveTemplateLanguage: %v", err)
	}
	if language != langSwift {
		t.Fatalf("language = %q, want %q", language, langSwift)
	}

	_, err = resolveTemplateLanguage(targetDarwin, "mac-llm", meta, initOptions{
		language:    langPython,
		languageSet: true,
	})
	if err == nil {
		t.Fatal("expected Python Darwin template language to fail")
	}
	if got, want := err.Error(), `template "mac-llm" is not available for language "python" (available: swift)`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestResolveTemplateLanguage_AcceptsAvailableTemplateLanguage(t *testing.T) {
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "realsense-camera", Languages: []string{langPython}},
		},
		Languages: []repoMetaLanguage{
			{Key: langPython, Name: "Python"},
			{Key: langSwift, Name: "Swift"},
		},
	}

	language, err := resolveTemplateLanguage(targetWendyOS, "realsense-camera", meta, initOptions{
		language:    langPython,
		languageSet: true,
	})
	if err != nil {
		t.Fatalf("resolveTemplateLanguage: %v", err)
	}
	if language != langPython {
		t.Fatalf("language = %q, want %q", language, langPython)
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
	if cfg.Platform != appconfig.PlatformLinux {
		t.Fatalf("Platform = %q, want %q", cfg.Platform, appconfig.PlatformLinux)
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

// NOTE: Native Mac end-to-end deployment requires a Wendy Agent for Mac target
// in CI. Until that exists, keep Darwin coverage at the CLI/config boundary here
// and validate real Mac deploys manually with the companion templates PR.
func TestInitCommand_NonInteractiveDarwinCreatesNativeSwiftProject(t *testing.T) {
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
		"--app-id", "mac-app",
		"--target", "macos",
		"--language", "swift",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.Platform != appconfig.PlatformDarwin {
		t.Fatalf("Platform = %q, want %q", cfg.Platform, appconfig.PlatformDarwin)
	}
	if cfg.Language != langSwift {
		t.Fatalf("Language = %q, want %q", cfg.Language, langSwift)
	}
	if len(cfg.Entitlements) != 0 {
		t.Fatalf("Entitlements = %+v, want none for native macOS", cfg.Entitlements)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "Package.swift")); err != nil {
		t.Fatalf("expected Package.swift: %v", err)
	}
}

func TestBuildInitEntitlementsFromFlags_RejectsDarwinEntitlements(t *testing.T) {
	_, err := buildInitEntitlementsFromFlags(targetDarwin, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"network"},
	})
	if err == nil {
		t.Fatal("expected Darwin entitlements to fail")
	}
	if got, want := err.Error(), `darwin apps do not support WendyOS container entitlements`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
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

	// Also replace the (equally interactive) ROS 2 framework prompt this test
	// doesn't care about, so it doesn't try to open a real TTY.
	origAskFrameworks := askFrameworkQuestions
	askFrameworkQuestions = func() (*appconfig.FrameworksConfig, error) { return nil, nil }
	t.Cleanup(func() { askFrameworkQuestions = origAskFrameworks })

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

func writeTemplateWendyJSON(t *testing.T, content string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "wendy.json")
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing wendy.json: %v", err)
	}
	return cfgPath
}

func readEntitlements(t *testing.T, cfgPath string) []appconfig.Entitlement {
	t.Helper()
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	return cfg.Entitlements
}

// The WDY-1810 repro: --template simple-api --entitlement gpu must produce a
// wendy.json containing both the template's network entitlement and gpu.
func TestMergeTemplateEntitlements_AddsFlagEntitlementToTemplateConfig(t *testing.T) {
	cfgPath := writeTemplateWendyJSON(t, `{
  "appId": "autotest-gpu",
  "version": "0.1.0",
  "platform": "linux",
  "language": "python",
  "entitlements": [{"type": "network"}]
}`)

	requested, err := templateEntitlementsFromFlags(targetWendyOS, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"gpu"},
	})
	if err != nil {
		t.Fatalf("templateEntitlementsFromFlags: %v", err)
	}

	added, err := mergeTemplateEntitlements(cfgPath, requested)
	if err != nil {
		t.Fatalf("mergeTemplateEntitlements: %v", err)
	}
	if len(added) != 1 || added[0] != appconfig.EntitlementGPU {
		t.Fatalf("added = %v, want [gpu]", added)
	}

	entitlements := readEntitlements(t, cfgPath)
	gotTypes := map[string]bool{}
	for _, ent := range entitlements {
		gotTypes[ent.Type] = true
	}
	if !gotTypes[appconfig.EntitlementNetwork] || !gotTypes[appconfig.EntitlementGPU] {
		t.Fatalf("entitlements = %+v, want network and gpu", entitlements)
	}
}

func TestMergeTemplateEntitlements_NoFlagsIsNoOp(t *testing.T) {
	requested, err := templateEntitlementsFromFlags(targetWendyOS, initOptions{})
	if err != nil {
		t.Fatalf("templateEntitlementsFromFlags: %v", err)
	}
	if requested != nil {
		t.Fatalf("requested = %+v, want nil", requested)
	}

	// No requested entitlements: the file must not even be read.
	added, err := mergeTemplateEntitlements(filepath.Join(t.TempDir(), "missing", "wendy.json"), nil)
	if err != nil {
		t.Fatalf("mergeTemplateEntitlements: %v", err)
	}
	if added != nil {
		t.Fatalf("added = %v, want nil", added)
	}
}

func TestMergeTemplateEntitlements_MissingConfigFails(t *testing.T) {
	_, err := mergeTemplateEntitlements(
		filepath.Join(t.TempDir(), "wendy.json"),
		[]appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	)
	if err == nil {
		t.Fatal("expected merge into a missing wendy.json to fail")
	}
}

func TestMergeTemplateEntitlements_CoveredEntitlementsLeaveFileUntouched(t *testing.T) {
	content := `{
  "appId": "demo",
  "entitlements": [{"type": "network", "mode": "host"}, {"type": "gpu"}]
}`
	cfgPath := writeTemplateWendyJSON(t, content)

	added, err := mergeTemplateEntitlements(cfgPath, []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
		{Type: appconfig.EntitlementGPU},
	})
	if err != nil {
		t.Fatalf("mergeTemplateEntitlements: %v", err)
	}
	if added != nil {
		t.Fatalf("added = %v, want nil", added)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Fatalf("wendy.json was rewritten:\n%s", data)
	}
}

func TestMergeTemplateEntitlements_PreservesUnknownKeysAndTemplateEntries(t *testing.T) {
	cfgPath := writeTemplateWendyJSON(t, `{
  "appId": "demo",
  "futureKey": {"nested": true},
  "entitlements": [{"type": "network", "mode": "host", "futureEntKey": 7}]
}`)

	added, err := mergeTemplateEntitlements(cfgPath, []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
		{Type: appconfig.EntitlementAudio},
	})
	if err != nil {
		t.Fatalf("mergeTemplateEntitlements: %v", err)
	}
	if len(added) != 1 || added[0] != appconfig.EntitlementAudio {
		t.Fatalf("added = %v, want [audio]", added)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["futureKey"]; !ok {
		t.Fatalf("unknown top-level key dropped: %s", data)
	}
	if !strings.Contains(string(raw["entitlements"]), "futureEntKey") {
		t.Fatalf("unknown entitlement key dropped: %s", raw["entitlements"])
	}

	entitlements := readEntitlements(t, cfgPath)
	if len(entitlements) != 2 {
		t.Fatalf("entitlements = %+v, want template network + audio", entitlements)
	}
	if entitlements[0].Type != appconfig.EntitlementNetwork || entitlements[0].Mode != "host" {
		t.Fatalf("template network entry changed: %+v", entitlements[0])
	}
	if entitlements[1].Type != appconfig.EntitlementAudio {
		t.Fatalf("entitlements[1] = %+v, want audio", entitlements[1])
	}
}

func TestMergeTemplateEntitlements_PersistDedupedByName(t *testing.T) {
	cfgPath := writeTemplateWendyJSON(t, `{
  "appId": "demo",
  "entitlements": [
    {"type": "network"},
    {"type": "persist", "name": "data", "path": "/data"}
  ]
}`)

	added, err := mergeTemplateEntitlements(cfgPath, []appconfig.Entitlement{
		{Type: appconfig.EntitlementPersist, Name: "data", Path: "/other"},
		{Type: appconfig.EntitlementPersist, Name: "cache", Path: "/cache"},
	})
	if err != nil {
		t.Fatalf("mergeTemplateEntitlements: %v", err)
	}
	if len(added) != 1 || added[0] != appconfig.EntitlementPersist {
		t.Fatalf("added = %v, want [persist]", added)
	}

	entitlements := readEntitlements(t, cfgPath)
	var persistNames []string
	for _, ent := range entitlements {
		if ent.Type == appconfig.EntitlementPersist {
			persistNames = append(persistNames, ent.Name)
		}
	}
	if len(persistNames) != 2 || persistNames[0] != "data" || persistNames[1] != "cache" {
		t.Fatalf("persist names = %v, want [data cache]", persistNames)
	}
}

func TestTemplateEntitlementCovers_GPIOPins(t *testing.T) {
	allPins := []appconfig.Entitlement{{Type: appconfig.EntitlementGPIO}}
	somePins := []appconfig.Entitlement{{Type: appconfig.EntitlementGPIO, Pins: []int{17, 27}}}

	req := appconfig.Entitlement{Type: appconfig.EntitlementGPIO, Pins: []int{17}}
	if !templateEntitlementCovers(allPins, req) {
		t.Fatal("pinless template gpio entry should cover any requested pins")
	}
	if !templateEntitlementCovers(somePins, req) {
		t.Fatal("template gpio pins [17 27] should cover requested [17]")
	}

	req.Pins = []int{17, 22}
	if templateEntitlementCovers(somePins, req) {
		t.Fatal("template gpio pins [17 27] should not cover requested [17 22]")
	}
}

func TestTemplateEntitlementsFromFlags_DarwinRejectsEntitlementFlags(t *testing.T) {
	_, err := templateEntitlementsFromFlags(targetDarwin, initOptions{
		entitlementsSet: true,
		entitlements:    []string{"gpu"},
	})
	if err == nil {
		t.Fatal("expected darwin + --entitlement to fail")
	}
}

// ── Problem B: `wendy init --template` (and the target picker upstream of
// it) must degrade to a plain-text list instead of crashing on TTY open when
// no TTY is attached. ────────────────────────────────────────────────────

func TestResolveInitTarget_NonInteractiveWithoutFlagFails(t *testing.T) {
	orig := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = orig })

	_, err := resolveInitTarget(initOptions{})
	if err == nil {
		t.Fatal("expected non-interactive target resolution without --target to fail")
	}
	if !strings.Contains(err.Error(), "--target is required") {
		t.Fatalf("error = %q, want mention of --target", err)
	}
}

func TestResolveInitTarget_NonInteractiveWithFlagSucceeds(t *testing.T) {
	orig := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = orig })

	target, err := resolveInitTarget(initOptions{target: "wendyos", targetSet: true})
	if err != nil {
		t.Fatalf("resolveInitTarget: %v", err)
	}
	if target != targetWendyOS {
		t.Fatalf("target = %q, want %q", target, targetWendyOS)
	}
}

// The WDY-init-template-tty repro: `wendy init --template` with no TTY must
// print the available templates instead of crashing with
// "picker: could not open a new TTY".
func TestResolveBareTemplatePick_NonInteractivePrintsListAndErrors(t *testing.T) {
	orig := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = orig })

	// Two wendyos templates, so the single-template auto-select cannot kick in.
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "simple-api", Description: "Minimal HTTP API"},
			{Name: "fullstack", Description: "Full-stack starter"},
			{Name: "mac-llm", Description: "macOS-only template", Targets: []string{targetDarwin}},
		},
	}

	_, err := resolveBareTemplatePick(targetWendyOS, meta)
	if err == nil {
		t.Fatal("expected non-interactive bare --template to fail")
	}
	if !strings.Contains(err.Error(), "--template requires a value") {
		t.Fatalf("error = %q, want mention of --template", err)
	}
}

func TestResolveBareTemplatePick_SingleTemplateAutoSelected(t *testing.T) {
	stubNonInteractive(t)
	meta := &repoMeta{Templates: []repoMetaTemplate{{Name: "go2-rc", Description: "Go2 remote control"}}}
	name, err := resolveBareTemplatePick(targetWendyOS, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "go2-rc" {
		t.Fatalf("expected go2-rc, got %q", name)
	}
}

func TestResolveBareTemplatePick_NonInteractiveWithNoTemplatesReportsNoTemplates(t *testing.T) {
	orig := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = orig })

	// Only a darwin template exists, so the wendyos list is empty. The error
	// must name the real problem instead of blaming the missing --template
	// value and printing an empty list.
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "mac-llm", Description: "macOS-only template", Targets: []string{targetDarwin}},
		},
	}

	_, err := resolveBareTemplatePick(targetWendyOS, meta)
	if err == nil {
		t.Fatal("expected non-interactive bare --template with no templates to fail")
	}
	if !strings.Contains(err.Error(), "no templates available for "+targetWendyOS) {
		t.Fatalf("error = %q, want %q", err, "no templates available for "+targetWendyOS)
	}
}

func TestTemplateItemsForTarget_FiltersByTarget(t *testing.T) {
	meta := &repoMeta{
		Templates: []repoMetaTemplate{
			{Name: "simple-api", Description: "Minimal HTTP API"},
			{Name: "mac-llm", Description: "macOS-only template", Targets: []string{targetDarwin}},
		},
	}

	items := templateItemsForTarget(targetWendyOS, meta)
	if len(items) != 1 || items[0].Value.(string) != "simple-api" {
		t.Fatalf("templateItemsForTarget(wendyos) = %+v, want only simple-api", items)
	}

	items = templateItemsForTarget(targetDarwin, meta)
	if len(items) != 1 || items[0].Value.(string) != "mac-llm" {
		t.Fatalf("templateItemsForTarget(darwin) = %+v, want only mac-llm", items)
	}
}

// ── Problem A: `frameworks`/ROS 2 support must be discoverable from
// `wendy init` via --framework/--ros2-* flags or an interactive prompt. ────

func TestBuildInitFrameworksFromFlags_NoFlagsIsNoOp(t *testing.T) {
	frameworks, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{})
	if err != nil {
		t.Fatalf("buildInitFrameworksFromFlags: %v", err)
	}
	if frameworks != nil {
		t.Fatalf("frameworks = %+v, want nil", frameworks)
	}
}

func TestBuildInitFrameworksFromFlags_FrameworkROS2CreatesDefaultConfig(t *testing.T) {
	frameworks, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		frameworksSet: true,
		frameworks:    []string{"ros2"},
	})
	if err != nil {
		t.Fatalf("buildInitFrameworksFromFlags: %v", err)
	}
	if frameworks == nil || frameworks.ROS2 == nil {
		t.Fatalf("frameworks = %+v, want a ros2 config", frameworks)
	}
	if frameworks.ROS2.DomainID != nil || frameworks.ROS2.RMW != "" || frameworks.ROS2.Distro != "" || frameworks.ROS2.DiscoveryScope != "" {
		t.Fatalf("ros2 config = %+v, want all fields left at their zero value (defaults apply)", frameworks.ROS2)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsUnknownFramework(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		frameworksSet: true,
		frameworks:    []string{"ros1"},
	})
	if err == nil {
		t.Fatal("expected unknown framework to fail")
	}
	if !strings.Contains(err.Error(), `"ros1"`) {
		t.Fatalf("error = %q, want mention of ros1", err)
	}
}

func TestBuildInitFrameworksFromFlags_EmptyFrameworkFlagFails(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		frameworksSet: true,
		frameworks:    []string{"", "  "},
	})
	if err == nil {
		t.Fatal("expected --framework with no valid entries to fail")
	}
}

// Ros2-specific flags imply the ros2 framework even without --framework ros2,
// mirroring how --gpio-pins implies the gpio entitlement.
func TestBuildInitFrameworksFromFlags_ROS2FlagsImplyFramework(t *testing.T) {
	domainID := 42
	frameworks, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		ros2DomainIDSet:       true,
		ros2DomainID:          domainID,
		ros2RMWSet:            true,
		ros2RMW:               "fastrtps",
		ros2DistroSet:         true,
		ros2Distro:            "jazzy",
		ros2DiscoveryScopeSet: true,
		ros2DiscoveryScope:    "host",
	})
	if err != nil {
		t.Fatalf("buildInitFrameworksFromFlags: %v", err)
	}
	if frameworks == nil || frameworks.ROS2 == nil {
		t.Fatal("expected ros2-specific flags to produce a ros2 config")
	}
	ros2 := frameworks.ROS2
	if ros2.DomainID == nil || *ros2.DomainID != domainID {
		t.Fatalf("DomainID = %v, want %d", ros2.DomainID, domainID)
	}
	if ros2.RMW != "fastrtps" {
		t.Fatalf("RMW = %q, want fastrtps", ros2.RMW)
	}
	if ros2.Distro != "jazzy" {
		t.Fatalf("Distro = %q, want jazzy", ros2.Distro)
	}
	if ros2.DiscoveryScope != "host" {
		t.Fatalf("DiscoveryScope = %q, want host", ros2.DiscoveryScope)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsOutOfRangeDomainID(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		ros2DomainIDSet: true,
		ros2DomainID:    9999,
	})
	if err == nil {
		t.Fatal("expected out-of-range --ros2-domain-id to fail")
	}
	if !strings.Contains(err.Error(), "--ros2-domain-id") {
		t.Fatalf("error = %q, want mention of --ros2-domain-id", err)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsInvalidRMW(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		ros2RMWSet: true,
		ros2RMW:    "not-a-real-rmw",
	})
	if err == nil {
		t.Fatal("expected invalid --ros2-rmw to fail")
	}
	if !strings.Contains(err.Error(), "--ros2-rmw") {
		t.Fatalf("error = %q, want mention of --ros2-rmw", err)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsInvalidDistro(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		ros2DistroSet: true,
		ros2Distro:    "Not_Valid!",
	})
	if err == nil {
		t.Fatal("expected invalid --ros2-distro to fail")
	}
	if !strings.Contains(err.Error(), "--ros2-distro") {
		t.Fatalf("error = %q, want mention of --ros2-distro", err)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsInvalidDiscoveryScope(t *testing.T) {
	_, err := buildInitFrameworksFromFlags(targetWendyOS, initOptions{
		ros2DiscoveryScopeSet: true,
		ros2DiscoveryScope:    "everywhere",
	})
	if err == nil {
		t.Fatal("expected invalid --ros2-discovery-scope to fail")
	}
	if !strings.Contains(err.Error(), "--ros2-discovery-scope") {
		t.Fatalf("error = %q, want mention of --ros2-discovery-scope", err)
	}
}

func TestBuildInitFrameworksFromFlags_RejectsUnsupportedTargets(t *testing.T) {
	for _, target := range []string{targetWendyLite, targetDarwin} {
		t.Run(target, func(t *testing.T) {
			_, err := buildInitFrameworksFromFlags(target, initOptions{
				frameworksSet: true,
				frameworks:    []string{"ros2"},
			})
			if err == nil {
				t.Fatalf("expected %s + ros2 framework to fail", target)
			}
		})
	}
}

func TestResolveInitFrameworks_SkipsInteractivePromptWhenEntitlementFlagsProvided(t *testing.T) {
	// If askFrameworkQuestions were invoked here, it would try to open a real
	// TTY (it isn't stubbed in this test) and fail; a nil, nil result proves
	// resolveInitFrameworks took the flag-driven no-op path instead.
	frameworks, err := resolveInitFrameworks(targetWendyOS, initOptions{noExtraEntitlements: true})
	if err != nil {
		t.Fatalf("resolveInitFrameworks: %v", err)
	}
	if frameworks != nil {
		t.Fatalf("frameworks = %+v, want nil", frameworks)
	}
}

func TestResolveInitFrameworks_SkipsInteractivePromptForUnsupportedTargets(t *testing.T) {
	for _, target := range []string{targetWendyLite, targetDarwin} {
		t.Run(target, func(t *testing.T) {
			frameworks, err := resolveInitFrameworks(target, initOptions{})
			if err != nil {
				t.Fatalf("resolveInitFrameworks: %v", err)
			}
			if frameworks != nil {
				t.Fatalf("frameworks = %+v, want nil", frameworks)
			}
		})
	}
}

func TestResolveInitFrameworks_UsesFlagsWhenProvided(t *testing.T) {
	frameworks, err := resolveInitFrameworks(targetWendyOS, initOptions{
		frameworksSet: true,
		frameworks:    []string{"ros2"},
	})
	if err != nil {
		t.Fatalf("resolveInitFrameworks: %v", err)
	}
	if frameworks == nil || frameworks.ROS2 == nil {
		t.Fatal("expected --framework ros2 to produce a ros2 config")
	}
}

func TestTemplateFrameworksFromFlags_NoFlagsIsNoOp(t *testing.T) {
	frameworks, err := templateFrameworksFromFlags(targetWendyOS, initOptions{})
	if err != nil {
		t.Fatalf("templateFrameworksFromFlags: %v", err)
	}
	if frameworks != nil {
		t.Fatalf("frameworks = %+v, want nil", frameworks)
	}
}

func TestMergeTemplateFrameworks_AddsROS2ToTemplateConfig(t *testing.T) {
	cfgPath := writeTemplateWendyJSON(t, `{
  "appId": "ros2-app",
  "version": "0.1.0",
  "platform": "linux",
  "language": "swift",
  "entitlements": [{"type": "network"}]
}`)

	requested, err := templateFrameworksFromFlags(targetWendyOS, initOptions{
		frameworksSet: true,
		frameworks:    []string{"ros2"},
	})
	if err != nil {
		t.Fatalf("templateFrameworksFromFlags: %v", err)
	}

	added, err := mergeTemplateFrameworks(cfgPath, requested)
	if err != nil {
		t.Fatalf("mergeTemplateFrameworks: %v", err)
	}
	if !added {
		t.Fatal("expected mergeTemplateFrameworks to report a change")
	}

	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if cfg.Frameworks == nil || cfg.Frameworks.ROS2 == nil {
		t.Fatalf("Frameworks = %+v, want a ros2 config", cfg.Frameworks)
	}
}

func TestMergeTemplateFrameworks_NoFlagsIsNoOp(t *testing.T) {
	added, err := mergeTemplateFrameworks(filepath.Join(t.TempDir(), "missing", "wendy.json"), nil)
	if err != nil {
		t.Fatalf("mergeTemplateFrameworks: %v", err)
	}
	if added {
		t.Fatal("expected no-op when nothing was requested")
	}
}

// The template's own "frameworks" config wins over --framework/--ros2-*
// flags, mirroring how a template's more specific entitlement config wins in
// mergeTemplateEntitlements.
func TestMergeTemplateFrameworks_TemplateConfigWins(t *testing.T) {
	content := `{
  "appId": "ros2-app",
  "frameworks": {"ros2": {"distro": "iron"}},
  "entitlements": [{"type": "network"}]
}`
	cfgPath := writeTemplateWendyJSON(t, content)

	domainID := 5
	requested := &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &domainID}}
	added, err := mergeTemplateFrameworks(cfgPath, requested)
	if err != nil {
		t.Fatalf("mergeTemplateFrameworks: %v", err)
	}
	if added {
		t.Fatal("expected template's existing frameworks config to win")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Fatalf("wendy.json was rewritten:\n%s", data)
	}
}

func TestMergeTemplateFrameworks_EmptyTemplateObjectDoesNotWin(t *testing.T) {
	// A template that writes "frameworks": {} (or a null member) configures
	// nothing, so it must not silently swallow --framework/--ros2-* flags.
	for name, content := range map[string]string{
		"emptyObject": `{"appId": "ros2-app", "frameworks": {}}`,
		"nullMember":  `{"appId": "ros2-app", "frameworks": {"ros2": null}}`,
		"nullValue":   `{"appId": "ros2-app", "frameworks": null}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath := writeTemplateWendyJSON(t, content)

			domainID := 5
			requested := &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &domainID}}
			added, err := mergeTemplateFrameworks(cfgPath, requested)
			if err != nil {
				t.Fatalf("mergeTemplateFrameworks: %v", err)
			}
			if !added {
				t.Fatal("expected requested frameworks to be merged into an unset template frameworks key")
			}

			data, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			var cfg appconfig.AppConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if cfg.Frameworks == nil || cfg.Frameworks.ROS2 == nil || cfg.Frameworks.ROS2.DomainID == nil {
				t.Fatalf("frameworks.ros2.domainId missing:\n%s", data)
			}
			if *cfg.Frameworks.ROS2.DomainID != domainID {
				t.Fatalf("domainId = %d, want %d", *cfg.Frameworks.ROS2.DomainID, domainID)
			}
		})
	}
}

func TestMergeTemplateFrameworks_MalformedFrameworksValueLeavesFileUntouched(t *testing.T) {
	// A scalar under "frameworks" is not something this merge can interpret,
	// so it is left alone rather than silently overwritten.
	content := `{"appId": "ros2-app", "frameworks": "ros2"}`
	cfgPath := writeTemplateWendyJSON(t, content)

	added, err := mergeTemplateFrameworks(cfgPath, &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}})
	if err != nil {
		t.Fatalf("mergeTemplateFrameworks: %v", err)
	}
	if added {
		t.Fatal("expected an uninterpretable frameworks value to be left untouched")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Fatalf("wendy.json was rewritten:\n%s", data)
	}
}

func TestMergeTemplateFrameworks_MissingConfigFails(t *testing.T) {
	_, err := mergeTemplateFrameworks(
		filepath.Join(t.TempDir(), "wendy.json"),
		&appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}},
	)
	if err == nil {
		t.Fatal("expected merge into a missing wendy.json to fail")
	}
}

// End-to-end: --framework ros2 plus --ros2-* flags on a non-template `wendy
// init` must produce a wendy.json with a populated "frameworks" key — the
// concrete, scriptable path around the discoverability gap in Problem A.
func TestInitCommand_FrameworkFlagsCreateROS2Config(t *testing.T) {
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
		"--app-id", "go2-network-bridge",
		"--target", "wendyos",
		"--language", "swift",
		"--no-extra-entitlements",
		"--framework", "ros2",
		"--ros2-rmw", "fastrtps",
		"--ros2-discovery-scope", "host",
		"--ros2-domain-id", "17",
		"--assistant", "skip",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := appconfig.LoadFromFile(filepath.Join(tempDir, "wendy.json"))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.Frameworks == nil || cfg.Frameworks.ROS2 == nil {
		t.Fatalf("Frameworks = %+v, want a ros2 config", cfg.Frameworks)
	}
	ros2 := cfg.Frameworks.ROS2
	if ros2.RMW != "fastrtps" {
		t.Fatalf("RMW = %q, want fastrtps", ros2.RMW)
	}
	if ros2.DiscoveryScope != "host" {
		t.Fatalf("DiscoveryScope = %q, want host", ros2.DiscoveryScope)
	}
	if ros2.DomainID == nil || *ros2.DomainID != 17 {
		t.Fatalf("DomainID = %v, want 17", ros2.DomainID)
	}
}

func TestInitCommand_NoFrameworkFlagsLeavesFrameworksNil(t *testing.T) {
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
		"--app-id", "plain-app",
		"--target", "wendyos",
		"--language", "swift",
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
	if cfg.Frameworks != nil {
		t.Fatalf("Frameworks = %+v, want nil when no --framework flags were passed", cfg.Frameworks)
	}
}

func TestTemplateTargets_DefaultsToWendyOS(t *testing.T) {
	targets := templateTargets(repoMetaTemplate{Name: "go2-rc"})
	if len(targets) != 1 || targets[0] != targetWendyOS {
		t.Fatalf("expected [%s], got %v", targetWendyOS, targets)
	}
}

func TestTemplateTargets_DropsUnknownTargets(t *testing.T) {
	targets := templateTargets(repoMetaTemplate{Name: "x", Targets: []string{"windows", targetDarwin}})
	if len(targets) != 1 || targets[0] != targetDarwin {
		t.Fatalf("expected [%s], got %v", targetDarwin, targets)
	}
}

func TestInitTargetItemsFor_ReusesSharedItems(t *testing.T) {
	items := initTargetItemsFor([]string{targetWendyOS, targetDarwin})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Name != "WendyOS" || items[1].Name != "macOS" {
		t.Fatalf("expected canonical initTargetItems entries, got %+v", items)
	}
}

func TestInitTargetDisplayName(t *testing.T) {
	if got := initTargetDisplayName(targetWendyOS); got != "WendyOS" {
		t.Fatalf("expected WendyOS, got %q", got)
	}
	if got := initTargetDisplayName("weird"); got != "weird" {
		t.Fatalf("expected raw passthrough, got %q", got)
	}
}

func TestResolveInitTargetForTemplate_SingleTargetSkipsPicker(t *testing.T) {
	stubNonInteractive(t) // success while non-interactive proves no picker ran
	target, err := resolveInitTargetForTemplate(repoMetaTemplate{Name: "go2-rc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != targetWendyOS {
		t.Fatalf("expected %s, got %q", targetWendyOS, target)
	}
}

func TestResolveInitTargetForTemplate_ExplicitSingleTarget(t *testing.T) {
	stubNonInteractive(t)
	target, err := resolveInitTargetForTemplate(repoMetaTemplate{Name: "mac-llm", Targets: []string{targetDarwin}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != targetDarwin {
		t.Fatalf("expected %s, got %q", targetDarwin, target)
	}
}

func TestResolveInitTargetForTemplate_NoSupportedTargetsErrors(t *testing.T) {
	_, err := resolveInitTargetForTemplate(repoMetaTemplate{Name: "x", Targets: []string{"windows"}})
	if err == nil || !strings.Contains(err.Error(), "not available for any supported target") {
		t.Fatalf("expected unsupported-target error, got: %v", err)
	}
}

func TestResolveInitTargetForTemplate_MultiTargetNonInteractiveRequiresTarget(t *testing.T) {
	stubNonInteractive(t)
	_, err := resolveInitTargetForTemplate(repoMetaTemplate{Name: "x", Targets: []string{targetWendyOS, targetDarwin}})
	if err == nil || !strings.Contains(err.Error(), "--target is required") {
		t.Fatalf("expected --target required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), targetWendyOS) || !strings.Contains(err.Error(), targetDarwin) {
		t.Fatalf("error should list the template's targets, got: %v", err)
	}
}

// stubFetchRepoMeta replaces the template-registry fetch with a canned result
// so tests never hit the network.
func stubFetchRepoMeta(t *testing.T, meta *repoMeta, err error) {
	t.Helper()
	orig := fetchRepoMetaWithUI
	fetchRepoMetaWithUI = func(branch string) (*repoMeta, error) { return meta, err }
	t.Cleanup(func() { fetchRepoMetaWithUI = orig })
}

func TestResolveInitTargetAndTemplate_SingleTargetTemplateInfersTarget(t *testing.T) {
	stubNonInteractive(t)
	meta := &repoMeta{
		Templates: []repoMetaTemplate{{Name: "go2-rc", Languages: []string{"python"}}},
		Languages: []repoMetaLanguage{{Key: "python", Name: "Python"}},
	}
	stubFetchRepoMeta(t, meta, nil)

	target, tmpl, gotMeta, err := resolveInitTargetAndTemplate(initOptions{template: "go2-rc", templateSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != targetWendyOS || tmpl != "go2-rc" || gotMeta != meta {
		t.Fatalf("expected (wendyos, go2-rc, meta), got (%q, %q, %v)", target, tmpl, gotMeta)
	}
}

func TestResolveInitTargetAndTemplate_MetaFetchErrorFails(t *testing.T) {
	stubNonInteractive(t)
	stubFetchRepoMeta(t, nil, fmt.Errorf("registry unreachable"))
	_, _, _, err := resolveInitTargetAndTemplate(initOptions{template: "go2-rc", templateSet: true})
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("expected fetch error to propagate, got: %v", err)
	}
}

func TestResolveInitTargetAndTemplate_UnknownTemplateFailsBeforeTargetPrompt(t *testing.T) {
	stubNonInteractive(t)
	stubFetchRepoMeta(t, &repoMeta{
		Templates: []repoMetaTemplate{{Name: "go2-rc"}},
	}, nil)
	_, _, _, err := resolveInitTargetAndTemplate(initOptions{template: "nope", templateSet: true})
	if err == nil || !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Fatalf("expected unknown-template error, got: %v", err)
	}
	if strings.Contains(err.Error(), "--target is required") {
		t.Fatalf("unknown template must be reported before any target requirement, got: %v", err)
	}
}

func TestResolveInitTargetAndTemplate_BareTemplateStillResolvesTargetFirst(t *testing.T) {
	stubNonInteractive(t)
	_, _, _, err := resolveInitTargetAndTemplate(initOptions{template: bareTemplatePickSentinel, templateSet: true})
	if err == nil || !strings.Contains(err.Error(), "--target is required when running non-interactively") {
		t.Fatalf("bare --template must fall through to target resolution, got: %v", err)
	}
}

func TestResolveInitTargetAndTemplate_TargetFlagKeepsExistingValidation(t *testing.T) {
	stubNonInteractive(t)
	stubFetchRepoMeta(t, &repoMeta{
		Templates: []repoMetaTemplate{{Name: "go2-rc"}}, // WendyOS-only
	}, nil)
	_, _, _, err := resolveInitTargetAndTemplate(initOptions{
		template: "go2-rc", templateSet: true,
		target: targetDarwin, targetSet: true,
	})
	if err == nil || !strings.Contains(err.Error(), "is not available for target") {
		t.Fatalf("expected existing target-mismatch error, got: %v", err)
	}
}

func TestResolveTemplateLanguage_SingleLanguageAutoSelected(t *testing.T) {
	stubNonInteractive(t) // success while non-interactive proves no picker ran
	meta := &repoMeta{
		Templates: []repoMetaTemplate{{Name: "go2-rc", Languages: []string{"python"}}},
		Languages: []repoMetaLanguage{{Key: "python", Name: "Python"}},
	}
	lang, err := resolveTemplateLanguage(targetWendyOS, "go2-rc", meta, initOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lang != langPython {
		t.Fatalf("expected python, got %q", lang)
	}
}

func TestInitCommand_NonInteractiveTemplateInfersTarget(t *testing.T) {
	stubNonInteractive(t)
	stubFetchRepoMeta(t, &repoMeta{
		Templates: []repoMetaTemplate{{Name: "go2-rc", Languages: []string{"python"}}},
		Languages: []repoMetaLanguage{{Key: "python", Name: "Python"}},
	}, nil)

	cmd := newInitCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--template", "go2-rc"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an app-ID error, got success")
	}
	if !strings.Contains(err.Error(), "an app ID is required") {
		t.Fatalf("expected the run to reach the app-ID step (target inferred, no --target error), got: %v", err)
	}
}

func TestResolveTemplateLanguage_NonInteractiveMultiLanguageRequiresFlag(t *testing.T) {
	stubNonInteractive(t)
	meta := &repoMeta{
		Templates: []repoMetaTemplate{{Name: "multi", Languages: []string{"python", "rust"}}},
		Languages: []repoMetaLanguage{{Key: "python", Name: "Python"}, {Key: "rust", Name: "Rust"}},
	}
	_, err := resolveTemplateLanguage(targetWendyOS, "multi", meta, initOptions{})
	if err == nil || !strings.Contains(err.Error(), "--language is required when running non-interactively") {
		t.Fatalf("expected --language required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "python") || !strings.Contains(err.Error(), "rust") {
		t.Fatalf("error should list the template's languages, got: %v", err)
	}
}

func TestResolveTemplateLanguage_DarwinMojo(t *testing.T) {
	meta := &repoMeta{Templates: []repoMetaTemplate{{Name: "mac-llm", Languages: []string{langSwift, langMojo}, Targets: []string{targetDarwin}}}, Languages: []repoMetaLanguage{{Key: langSwift, Name: "Swift"}, {Key: langMojo, Name: "Mojo"}}}
	language, err := resolveTemplateLanguage(targetDarwin, "mac-llm", meta, initOptions{language: langMojo, languageSet: true})
	if err != nil || language != langMojo {
		t.Fatalf("language=%q error=%v", language, err)
	}
}
