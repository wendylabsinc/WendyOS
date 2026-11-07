package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// ---------------------------------------------------------------------------
// runXcodebuild
// ---------------------------------------------------------------------------

func TestRunXcodebuild_CreatesLogFile(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Write a known string to stdout so we can verify it ends up in the log.
		return exec.CommandContext(ctx, "sh", "-c", `echo "xcodebuild output"`)
	}

	if err := runXcodebuild(context.Background(), dir, "-project", "X.xcodeproj"); err != nil {
		t.Fatalf("runXcodebuild error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".xcode", "xcodebuild.log"))
	if err != nil {
		t.Fatalf("xcodebuild.log not created: %v", err)
	}
	if !strings.Contains(string(data), "xcodebuild output") {
		t.Errorf("xcodebuild.log does not contain command output; got:\n%s", data)
	}
}

func TestRunXcodebuild_LogFileContainsHeader(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}

	if err := runXcodebuild(context.Background(), dir, "-scheme", "MyScheme"); err != nil {
		t.Fatalf("runXcodebuild error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".xcode", "xcodebuild.log"))
	if err != nil {
		t.Fatalf("xcodebuild.log not created: %v", err)
	}
	// Header must contain the args and a timestamp.
	if !strings.Contains(string(data), "-scheme") {
		t.Errorf("xcodebuild.log header missing args; got:\n%s", data)
	}
}

func TestRunXcodebuild_TruncatesLogOnEachRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".xcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate the log with stale content.
	if err := os.WriteFile(filepath.Join(dir, ".xcode", "xcodebuild.log"), []byte("stale content"), 0o644); err != nil {
		t.Fatal(err)
	}

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}

	if err := runXcodebuild(context.Background(), dir); err != nil {
		t.Fatalf("runXcodebuild error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".xcode", "xcodebuild.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stale content") {
		t.Error("xcodebuild.log still contains stale content from previous run")
	}
}

func TestRunXcodebuild_ReturnsErrorOnFailure(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}

	if err := runXcodebuild(context.Background(), dir); err == nil {
		t.Fatal("expected error on xcodebuild failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// looksLikeCLTOnlySelected / installedXcodeApps
// ---------------------------------------------------------------------------

func TestLooksLikeCLTOnlySelected(t *testing.T) {
	cltOutput := `xcodebuild: error: unable to find utility "xcodebuild", not a developer tool or in PATH
xcode-select: error: tool 'xcodebuild' requires Xcode, but active developer directory '/Library/Developer/CommandLineTools' is a command line tools instance`
	if !looksLikeCLTOnlySelected(cltOutput) {
		t.Error("expected CLT-only output to be detected")
	}
	if looksLikeCLTOnlySelected("xcodebuild: error: Scheme MyScheme is not currently configured") {
		t.Error("did not expect an unrelated xcodebuild error to be detected as CLT-only")
	}
	if looksLikeCLTOnlySelected("") {
		t.Error("did not expect empty output to be detected as CLT-only")
	}
}

func TestInstalledXcodeApps_FiltersToRealXcodeBundles(t *testing.T) {
	dir := t.TempDir()
	origAppsDir := applicationsDir
	applicationsDir = dir
	t.Cleanup(func() { applicationsDir = origAppsDir })

	// A real Xcode install: has Contents/Developer/usr/bin/xcodebuild.
	makeXcodeBundle(t, dir, "Xcode.app")
	makeXcodeBundle(t, dir, "Xcode-beta.app")
	// A stray folder that merely looks like an Xcode install (no xcodebuild inside).
	if err := os.MkdirAll(filepath.Join(dir, "NotXcode.app", "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Command Line Tools is not an .app bundle, so it never matches the glob;
	// nothing to special-case here.

	apps := installedXcodeApps()
	if len(apps) != 2 {
		t.Fatalf("installedXcodeApps = %v; want 2 entries", apps)
	}
	if filepath.Base(apps[0]) != "Xcode-beta.app" || filepath.Base(apps[1]) != "Xcode.app" {
		t.Errorf("installedXcodeApps = %v; want sorted [Xcode-beta.app Xcode.app]", apps)
	}
}

func TestInstalledXcodeApps_NoneInstalled(t *testing.T) {
	dir := t.TempDir()
	origAppsDir := applicationsDir
	applicationsDir = dir
	t.Cleanup(func() { applicationsDir = origAppsDir })

	if apps := installedXcodeApps(); len(apps) != 0 {
		t.Errorf("installedXcodeApps = %v; want empty", apps)
	}
}

// makeXcodeBundle creates a fake Xcode.app bundle under dir with just enough
// structure (Contents/Developer/usr/bin/xcodebuild) to satisfy installedXcodeApps.
func makeXcodeBundle(t *testing.T, dir, name string) string {
	t.Helper()
	binDir := filepath.Join(dir, name, "Contents", "Developer", "usr", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	xcodebuildPath := filepath.Join(binDir, "xcodebuild")
	if err := os.WriteFile(xcodebuildPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

// ---------------------------------------------------------------------------
// xcodeSelectGuidance
// ---------------------------------------------------------------------------

func TestXcodeSelectGuidance_NoXcodeInstalled(t *testing.T) {
	dir := t.TempDir()
	origAppsDir := applicationsDir
	applicationsDir = dir
	t.Cleanup(func() { applicationsDir = origAppsDir })

	err := xcodeSelectGuidance(context.Background())
	if err == nil {
		t.Fatal("expected guidance error when no Xcode is installed")
	}
	if !strings.Contains(err.Error(), "App Store") || !strings.Contains(err.Error(), "developer.apple.com") {
		t.Errorf("expected install guidance mentioning the App Store and developer.apple.com, got: %v", err)
	}
}

func TestXcodeSelectGuidance_NonInteractive_ListsCandidates(t *testing.T) {
	dir := t.TempDir()
	origAppsDir := applicationsDir
	applicationsDir = dir
	t.Cleanup(func() { applicationsDir = origAppsDir })
	makeXcodeBundle(t, dir, "Xcode.app")
	makeXcodeBundle(t, dir, "Xcode-beta.app")

	origInteractive := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = origInteractive })

	err := xcodeSelectGuidance(context.Background())
	if err == nil {
		t.Fatal("expected guidance error in non-interactive mode")
	}
	if !strings.Contains(err.Error(), "Xcode.app") || !strings.Contains(err.Error(), "Xcode-beta.app") {
		t.Errorf("expected both candidates named in the error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "xcode-select -s") {
		t.Errorf("expected the xcode-select -s command in the error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// findXcodeScheme CLT recovery
// ---------------------------------------------------------------------------

func TestFindXcodeScheme_RecoversFromCLTOnlySelected(t *testing.T) {
	origExec := execCommandContext
	t.Cleanup(func() { execCommandContext = origExec })
	origGuidance := xcodeSelectGuidanceFn
	t.Cleanup(func() { xcodeSelectGuidanceFn = origGuidance })

	calls := 0
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			script := `echo "xcode-select: error: tool 'xcodebuild' requires Xcode, but active developer directory '/Library/Developer/CommandLineTools' is a command line tools instance" >&2; exit 1`
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
		script := `echo '{"project":{"schemes":["HelloXcode"]}}'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	guidanceCalls := 0
	xcodeSelectGuidanceFn = func(ctx context.Context) error {
		guidanceCalls++
		return nil // simulate a successful xcode-select fix
	}

	scheme, err := findXcodeScheme(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("findXcodeScheme error: %v", err)
	}
	if scheme != "HelloXcode" {
		t.Errorf("findXcodeScheme = %q; want %q", scheme, "HelloXcode")
	}
	if calls != 2 {
		t.Errorf("expected 2 xcodebuild calls (fail + retry), got %d", calls)
	}
	if guidanceCalls != 1 {
		t.Errorf("expected xcodeSelectGuidanceFn to be called once, got %d", guidanceCalls)
	}
}

func TestFindXcodeScheme_CLTGuidanceErrorPropagates(t *testing.T) {
	origExec := execCommandContext
	t.Cleanup(func() { execCommandContext = origExec })
	origGuidance := xcodeSelectGuidanceFn
	t.Cleanup(func() { xcodeSelectGuidanceFn = origGuidance })

	calls := 0
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		script := `echo "xcode-select: error: tool 'xcodebuild' requires Xcode, but active developer directory '/Library/Developer/CommandLineTools' is a command line tools instance" >&2; exit 1`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	xcodeSelectGuidanceFn = func(ctx context.Context) error {
		return fmt.Errorf("install Xcode from the App Store")
	}

	_, err := findXcodeScheme(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected error to propagate from xcodeSelectGuidanceFn")
	}
	if !strings.Contains(err.Error(), "App Store") {
		t.Errorf("expected guidance error to propagate, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected only 1 xcodebuild call when guidance fails (no retry), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// buildXcodeProject
// ---------------------------------------------------------------------------

func TestBuildXcodeProject_SchemeFromWendyJSON(t *testing.T) {
	dir := t.TempDir()

	// Write a wendy.json with an explicit xcode.scheme override.
	cfg := map[string]any{"appId": "myapp", "xcode": map[string]string{"scheme": "MyScheme"}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var gotArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		return exec.CommandContext(ctx, "true")
	}

	if err := buildXcodeProject(context.Background(), dir, "MyApp.xcodeproj"); err != nil {
		t.Fatalf("buildXcodeProject error: %v", err)
	}

	// Scheme from wendy.json must be passed to xcodebuild.
	if len(gotArgs) == 0 || gotArgs[0] != "xcodebuild" {
		t.Fatalf("expected xcodebuild invocation, got %v", gotArgs)
	}
	schemeIdx := -1
	for i, a := range gotArgs {
		if a == "-scheme" && i+1 < len(gotArgs) {
			schemeIdx = i + 1
		}
	}
	if schemeIdx < 0 {
		t.Fatal("xcodebuild called without -scheme")
	}
	if gotArgs[schemeIdx] != "MyScheme" {
		t.Errorf("-scheme = %q; want %q", gotArgs[schemeIdx], "MyScheme")
	}
}

func TestBuildXcodeProject_SchemeAutoDetected(t *testing.T) {
	dir := t.TempDir()
	// No wendy.json — scheme must be auto-detected via xcodebuild -list.

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		// First call is xcodebuild -list -json (scheme discovery).
		if len(args) > 0 && args[0] == "-list" {
			script := `echo '{"project":{"schemes":["AutoScheme"]}}'`
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
		// Second call is the actual xcodebuild build.
		return exec.CommandContext(ctx, "true")
	}

	if err := buildXcodeProject(context.Background(), dir, "MyApp.xcodeproj"); err != nil {
		t.Fatalf("buildXcodeProject error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 xcodebuild calls (list + build), got %d: %v", len(calls), calls)
	}
	// Second call must pass the auto-detected scheme.
	buildCall := calls[1]
	schemeIdx := -1
	for i, a := range buildCall {
		if a == "-scheme" && i+1 < len(buildCall) {
			schemeIdx = i + 1
		}
	}
	if schemeIdx < 0 {
		t.Fatal("xcodebuild build call missing -scheme")
	}
	if buildCall[schemeIdx] != "AutoScheme" {
		t.Errorf("-scheme = %q; want %q", buildCall[schemeIdx], "AutoScheme")
	}
}

func TestBuildXcodeProject_CorrectFlags(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var buildArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "-list" {
			script := `echo '{"project":{"schemes":["S"]}}'`
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
		buildArgs = append([]string{name}, args...)
		return exec.CommandContext(ctx, "true")
	}

	const xp = "Hello.xcodeproj"
	if err := buildXcodeProject(context.Background(), dir, xp); err != nil {
		t.Fatalf("buildXcodeProject error: %v", err)
	}

	want := map[string]string{
		"-project":         xp,
		"-configuration":   "Release",
		"-derivedDataPath": ".xcode/",
	}
	for flag, val := range want {
		found := false
		for i, a := range buildArgs {
			if a == flag && i+1 < len(buildArgs) && buildArgs[i+1] == val {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("xcodebuild missing %s %s in args: %v", flag, val, buildArgs)
		}
	}
	for _, flag := range []string{"-skipMacroValidation", "-skipPackagePluginValidation"} {
		found := false
		for _, a := range buildArgs {
			if a == flag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("xcodebuild missing %s in args: %v", flag, buildArgs)
		}
	}
}

func TestBuildXcodeProject_XcodebuildMissing(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist-12345")
	}

	err := buildXcodeProject(context.Background(), dir, "MyApp.xcodeproj")
	if err == nil {
		t.Fatal("expected error when xcodebuild missing, got nil")
	}
	// Either the scheme-discovery step or the build step surfaces the error.
	if !strings.Contains(err.Error(), "xcodebuild") {
		t.Errorf("expected 'xcodebuild' in error, got: %v", err)
	}
}

func TestBuildXcodeProject_BuildFails(t *testing.T) {
	dir := t.TempDir()

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "-list" {
			script := `echo '{"project":{"schemes":["S"]}}'`
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
		return exec.CommandContext(ctx, "false")
	}

	err := buildXcodeProject(context.Background(), dir, "MyApp.xcodeproj")
	if err == nil {
		t.Fatal("expected error when xcodebuild fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// findXcodeProj
// ---------------------------------------------------------------------------

func TestFindXcodeProj_None(t *testing.T) {
	dir := t.TempDir()
	got, err := findXcodeProj(dir)
	if err != nil {
		t.Fatalf("findXcodeProj unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("findXcodeProj = %q; want empty string", got)
	}
}

func TestFindXcodeProj_One(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findXcodeProj(dir)
	if err != nil {
		t.Fatalf("findXcodeProj unexpected error: %v", err)
	}
	if got != "MyApp.xcodeproj" {
		t.Errorf("findXcodeProj = %q; want %q", got, "MyApp.xcodeproj")
	}
}

func TestFindXcodeProj_Multiple_Error(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"First.xcodeproj", "Second.xcodeproj"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := findXcodeProj(dir)
	if err == nil {
		t.Fatal("findXcodeProj expected error for multiple .xcodeproj dirs, got nil")
	}
	if !strings.Contains(err.Error(), "multiple .xcodeproj") {
		t.Errorf("expected 'multiple .xcodeproj' in error, got: %v", err)
	}
}

func TestFindXcodeProj_IgnoresFiles(t *testing.T) {
	// A regular file named *.xcodeproj should not be counted.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fake.xcodeproj"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findXcodeProj(dir)
	if err != nil {
		t.Fatalf("findXcodeProj unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("findXcodeProj = %q; want empty (file should be ignored)", got)
	}
}

// ---------------------------------------------------------------------------
// parseXcodeSchemes
// ---------------------------------------------------------------------------

func TestParseXcodeSchemes_Project(t *testing.T) {
	data := []byte(`{
		"project": {
			"configurations": ["Debug","Release"],
			"name": "HelloXcode",
			"schemes": ["HelloXcode","HelloXcodeTests"],
			"targets": ["HelloXcode"]
		}
	}`)
	schemes, err := parseXcodeSchemes(data)
	if err != nil {
		t.Fatalf("parseXcodeSchemes error: %v", err)
	}
	if len(schemes) != 2 || schemes[0] != "HelloXcode" || schemes[1] != "HelloXcodeTests" {
		t.Errorf("parseXcodeSchemes = %v; want [HelloXcode HelloXcodeTests]", schemes)
	}
}

func TestParseXcodeSchemes_Workspace(t *testing.T) {
	data := []byte(`{
		"workspace": {
			"name": "MyWorkspace",
			"schemes": ["AppScheme"]
		}
	}`)
	schemes, err := parseXcodeSchemes(data)
	if err != nil {
		t.Fatalf("parseXcodeSchemes error: %v", err)
	}
	if len(schemes) != 1 || schemes[0] != "AppScheme" {
		t.Errorf("parseXcodeSchemes = %v; want [AppScheme]", schemes)
	}
}

func TestParseXcodeSchemes_EmptySchemes(t *testing.T) {
	data := []byte(`{"project": {"schemes": []}}`)
	schemes, err := parseXcodeSchemes(data)
	if err != nil {
		t.Fatalf("parseXcodeSchemes error: %v", err)
	}
	if len(schemes) != 0 {
		t.Errorf("parseXcodeSchemes = %v; want empty", schemes)
	}
}

func TestParseXcodeSchemes_NeitherKey(t *testing.T) {
	data := []byte(`{"other": {}}`)
	_, err := parseXcodeSchemes(data)
	if err == nil {
		t.Fatal("parseXcodeSchemes expected error when neither project nor workspace key present")
	}
}

func TestParseXcodeSchemes_InvalidJSON(t *testing.T) {
	_, err := parseXcodeSchemes([]byte("not json"))
	if err == nil {
		t.Fatal("parseXcodeSchemes expected error for invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// findXcodeScheme (via execCommandContext injection)
// ---------------------------------------------------------------------------

func TestFindXcodeScheme_SingleScheme(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Return a command that writes valid JSON to stdout.
		script := `echo '{"project":{"schemes":["HelloXcode"]}}'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	scheme, err := findXcodeScheme(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("findXcodeScheme error: %v", err)
	}
	if scheme != "HelloXcode" {
		t.Errorf("findXcodeScheme = %q; want %q", scheme, "HelloXcode")
	}
}

func TestFindXcodeScheme_IgnoresStderrWarnings(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		script := `echo 'warning: using the first of multiple matching destinations:' >&2; echo '{"project":{"schemes":["HelloXcode"]}}'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	scheme, err := findXcodeScheme(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("findXcodeScheme error: %v", err)
	}
	if scheme != "HelloXcode" {
		t.Errorf("findXcodeScheme = %q; want %q", scheme, "HelloXcode")
	}
}

func TestFindXcodeScheme_MultipleSchemes_Error(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		script := `echo '{"project":{"schemes":["App","AppTests"]}}'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	_, err := findXcodeScheme(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("findXcodeScheme expected error for multiple schemes, got nil")
	}
	if !strings.Contains(err.Error(), "multiple schemes") {
		t.Errorf("expected 'multiple schemes' in error, got: %v", err)
	}
}

func TestFindXcodeScheme_NoSchemes_Error(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		script := `echo '{"project":{"schemes":[]}}'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	_, err := findXcodeScheme(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("findXcodeScheme expected error when no schemes found, got nil")
	}
}

func TestFindXcodeScheme_XcodebuildMissing(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist-12345")
	}

	_, err := findXcodeScheme(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("findXcodeScheme expected error when xcodebuild missing, got nil")
	}
}

// ---------------------------------------------------------------------------
// findXcodeBuildProduct
// ---------------------------------------------------------------------------

func TestFindXcodeBuildProduct_Binary(t *testing.T) {
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a fake binary.
	if err := os.WriteFile(filepath.Join(releaseDir, "MyCLI"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, isApp, err := findXcodeBuildProduct(derived, "MyCLI")
	if err != nil {
		t.Fatalf("findXcodeBuildProduct error: %v", err)
	}
	if isApp {
		t.Error("isApp should be false for plain binary")
	}
	if filepath.Base(path) != "MyCLI" {
		t.Errorf("product path base = %q; want %q", filepath.Base(path), "MyCLI")
	}
}

func TestFindXcodeBuildProduct_AppBundle(t *testing.T) {
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	appDir := filepath.Join(releaseDir, "MyApp.app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a fake binary inside the bundle.
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macosDir, "MyApp"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, isApp, err := findXcodeBuildProduct(derived, "MyApp")
	if err != nil {
		t.Fatalf("findXcodeBuildProduct error: %v", err)
	}
	if !isApp {
		t.Error("isApp should be true for .app bundle")
	}
	if filepath.Base(path) != "MyApp.app" {
		t.Errorf("product path base = %q; want %q", filepath.Base(path), "MyApp.app")
	}
}

func TestFindXcodeBuildProduct_AppBundleTakesPrecedenceOverBinary(t *testing.T) {
	// When both a .app dir and a plain binary exist, .app wins.
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	if err := os.MkdirAll(filepath.Join(releaseDir, "MyApp.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "MyApp"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, isApp, err := findXcodeBuildProduct(derived, "MyApp")
	if err != nil {
		t.Fatalf("findXcodeBuildProduct error: %v", err)
	}
	if !isApp {
		t.Error("isApp should be true when .app dir present alongside binary")
	}
}

func TestFindXcodeBuildProduct_NotFound(t *testing.T) {
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := findXcodeBuildProduct(derived, "Ghost")
	if err == nil {
		t.Fatal("findXcodeBuildProduct expected error when product not found, got nil")
	}
}

// ---------------------------------------------------------------------------
// xcodeEntrypoint
// ---------------------------------------------------------------------------

func TestXcodeEntrypoint(t *testing.T) {
	tests := []struct {
		productPath string
		isApp       bool
		want        string
	}{
		{"/path/to/Build/Products/Release/MyCLI", false, "MyCLI"},
		{"/path/to/Build/Products/Release/MyApp.app", true, "MyApp.app/Contents/MacOS/MyApp"},
		{"/path/to/Build/Products/Release/HelloXcode.app", true, "HelloXcode.app/Contents/MacOS/HelloXcode"},
		{"/path/to/Build/Products/Release/Tool", false, "Tool"},
	}
	for _, tt := range tests {
		got := xcodeEntrypoint(tt.productPath, tt.isApp)
		if got != tt.want {
			t.Errorf("xcodeEntrypoint(%q, %v) = %q; want %q", tt.productPath, tt.isApp, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// assembleXcodeSyncEntries
// ---------------------------------------------------------------------------

func TestAssembleXcodeSyncEntries_Binary(t *testing.T) {
	// Build a fake Release directory with a binary and a sibling .bundle.
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(releaseDir, "MyCLI")
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(releaseDir, "Resources.bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	cfg := &appconfig.AppConfig{AppID: "mycli"}

	entries, err := assembleXcodeSyncEntries(binPath, false, cwd, cfg)
	if err != nil {
		t.Fatalf("assembleXcodeSyncEntries error: %v", err)
	}

	// Expect binary + bundle entries.
	remotes := make(map[string]bool)
	for _, e := range entries {
		remotes[e.remotePath] = true
	}
	if !remotes["MyCLI"] {
		t.Error("expected binary entry with remotePath MyCLI")
	}
	if !remotes["Resources.bundle"] {
		t.Error("expected bundle entry with remotePath Resources.bundle")
	}
}

func TestAssembleXcodeSyncEntries_AppBundle(t *testing.T) {
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	appPath := filepath.Join(releaseDir, "MyApp.app")
	macosDir := filepath.Join(appPath, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	cfg := &appconfig.AppConfig{AppID: "myapp"}

	entries, err := assembleXcodeSyncEntries(appPath, true, cwd, cfg)
	if err != nil {
		t.Fatalf("assembleXcodeSyncEntries error: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for .app bundle, got %d", len(entries))
	}
	if entries[0].remotePath != "MyApp.app" {
		t.Errorf("remotePath = %q; want %q", entries[0].remotePath, "MyApp.app")
	}
	if entries[0].localPath != appPath {
		t.Errorf("localPath = %q; want %q", entries[0].localPath, appPath)
	}
}

func TestAssembleXcodeSyncEntries_SandboxAndFiles(t *testing.T) {
	derived := t.TempDir()
	releaseDir := filepath.Join(derived, "Build", "Products", "Release")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(releaseDir, "Tool")
	if err := os.WriteFile(binPath, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	// Create sandbox.sb in the project directory.
	if err := os.WriteFile(filepath.Join(cwd, "sandbox.sb"), []byte("(version 1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a user file.
	if err := os.WriteFile(filepath.Join(cwd, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create an explicit Brewfile outside the project root.
	if err := os.MkdirAll(filepath.Join(cwd, "ops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ops", "Brewfile"), []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &appconfig.AppConfig{
		AppID:    "tool",
		Files:    []appconfig.FileSyncEntry{{Path: "config.json"}},
		Brewfile: "ops/Brewfile",
	}

	entries, err := assembleXcodeSyncEntries(binPath, false, cwd, cfg)
	if err != nil {
		t.Fatalf("assembleXcodeSyncEntries error: %v", err)
	}

	remotes := make(map[string]bool)
	for _, e := range entries {
		remotes[e.remotePath] = true
	}
	if !remotes["sandbox.sb"] {
		t.Error("expected sandbox.sb entry")
	}
	if !remotes["config.json"] {
		t.Error("expected config.json entry from wendy.json Files")
	}
	if !remotes["ops/Brewfile"] {
		t.Error("expected explicit Brewfile entry")
	}
}

// ---------------------------------------------------------------------------
// runMacOSXcodeWithAgent
// ---------------------------------------------------------------------------

// countingVersionAgent counts GetAgentVersion calls.
type countingVersionAgent struct {
	agentpb.WendyAgentServiceClient
	calls int
}

func (a *countingVersionAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	a.calls++
	return &agentpb.GetAgentVersionResponse{CpuArchitecture: runtime.GOARCH}, nil
}

// runWithAgentRecorded fetches the device version just before an Xcode run,
// so the Xcode path must reuse it rather than ask the device again.
func TestRunMacOSXcodeWithAgentReusesCachedVersion(t *testing.T) {
	agent := &countingVersionAgent{}
	conn := &grpcclient.AgentConnection{AgentService: agent}
	// An architecture the host can never match stops the run at the first
	// check, and only the cached response carries it.
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: "cached-arch"})

	err := runMacOSXcodeWithAgent(context.Background(), conn, t.TempDir(), &appconfig.AppConfig{AppID: "app"}, runOptions{})
	if !errors.Is(err, errProjectTargetMismatch) || !strings.Contains(err.Error(), "cached-arch") {
		t.Fatalf("err = %v, want the architecture mismatch from the cached version", err)
	}
	if agent.calls != 0 {
		t.Fatalf("GetAgentVersion called %d times, want 0: the cached version should be reused", agent.calls)
	}
}
