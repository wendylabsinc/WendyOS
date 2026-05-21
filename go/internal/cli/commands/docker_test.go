package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
)

func mustDetectProjectType(t *testing.T, dir string) string {
	t.Helper()
	got, err := detectProjectType(dir)
	if err != nil {
		t.Fatalf("detectProjectType unexpected error: %v", err)
	}
	return got
}

func TestEnsureDockerDaemon_DarwinUsesBundledCLIWhenRuntimeInstalledButDockerMissingFromPath(t *testing.T) {
	oldRuntimes := darwinDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	oldOpenRuntime := dockerOpenRuntimeFn
	oldInstallRuntime := dockerInstallRuntimeFn
	oldInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		darwinDockerRuntimes = oldRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
		dockerOpenRuntimeFn = oldOpenRuntime
		dockerInstallRuntimeFn = oldInstallRuntime
		isInteractiveTerminalFn = oldInteractive
	})

	dir := t.TempDir()
	appPath := filepath.Join(dir, "Docker.app")
	cliPath := filepath.Join(appPath, "Contents", "Resources", "bin", "docker")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	darwinDockerRuntimes = []dockerRuntime{{
		name:        "Docker Desktop",
		app:         appPath,
		cliPaths:    []string{cliPath},
		cliLinkHint: "link Docker Desktop CLI tools",
	}}
	t.Setenv("PATH", filepath.Join(dir, "no-docker-on-path"))
	cliDir := filepath.Dir(cliPath)

	dockerLookPathFn = func(file string) (string, error) {
		if file == "docker" && pathHasDir(os.Getenv("PATH"), cliDir) {
			return cliPath, nil
		}
		return "", errors.New("not found")
	}
	versionChecks := 0
	dockerVersionOKFn = func(context.Context) bool {
		versionChecks++
		return pathHasDir(os.Getenv("PATH"), cliDir)
	}
	dockerOpenRuntimeFn = func(context.Context, string) error {
		t.Fatal("should not open Docker Desktop once bundled CLI works")
		return nil
	}
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("should not prompt/install Docker Desktop when the app is already installed")
		return nil
	}
	isInteractiveTerminalFn = func() bool { return false }

	if err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin); err != nil {
		t.Fatalf("ensureDockerDaemonForHostOS: %v", err)
	}
	if !pathHasDir(os.Getenv("PATH"), cliDir) {
		t.Fatalf("PATH = %q, want bundled CLI dir %q", os.Getenv("PATH"), cliDir)
	}
	if versionChecks < 2 {
		t.Fatalf("version checks = %d, want initial failure and retry after PATH update", versionChecks)
	}
}

func TestEnsureDockerDaemon_DarwinRuntimeInstalledButDockerCLIMissingDiagnostic(t *testing.T) {
	oldRuntimes := darwinDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	oldOpenRuntime := dockerOpenRuntimeFn
	oldInstallRuntime := dockerInstallRuntimeFn
	oldInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		darwinDockerRuntimes = oldRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
		dockerOpenRuntimeFn = oldOpenRuntime
		dockerInstallRuntimeFn = oldInstallRuntime
		isInteractiveTerminalFn = oldInteractive
	})

	dir := t.TempDir()
	appPath := filepath.Join(dir, "Docker.app")
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		t.Fatal(err)
	}

	darwinDockerRuntimes = []dockerRuntime{{
		name:        "Docker Desktop",
		app:         appPath,
		cliPaths:    []string{filepath.Join(appPath, "Contents", "Resources", "bin", "docker")},
		cliLinkHint: "link Docker Desktop CLI tools",
	}}
	t.Setenv("PATH", filepath.Join(dir, "no-docker-on-path"))
	dockerLookPathFn = func(string) (string, error) { return "", errors.New("not found") }
	dockerVersionOKFn = func(context.Context) bool { return false }
	dockerOpenRuntimeFn = func(context.Context, string) error {
		t.Fatal("should not open runtime when no docker CLI is available")
		return nil
	}
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("should not prompt/install Docker Desktop when the app is already installed")
		return nil
	}
	isInteractiveTerminalFn = func() bool { return false }

	err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin)
	if err == nil {
		t.Fatal("expected docker CLI missing diagnostic")
	}
	msg := err.Error()
	for _, want := range []string{"Docker Desktop is installed", "docker CLI is not on PATH", "link Docker Desktop CLI tools"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want substring %q", msg, want)
		}
	}
}

func TestEnsureDockerDaemon_WindowsUsesBundledCLIWhenDockerDesktopInstalledButDockerMissingFromPath(t *testing.T) {
	oldWindowsRuntimes := windowsDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	t.Cleanup(func() {
		windowsDockerRuntimes = oldWindowsRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
	})

	dir := t.TempDir()
	appPath := filepath.Join(dir, "Docker Desktop.exe")
	cliPath := filepath.Join(dir, "resources", "bin", "docker.exe")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appPath, []byte("exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, []byte("exe"), 0o755); err != nil {
		t.Fatal(err)
	}

	windowsDockerRuntimes = []dockerRuntime{{
		name:        "Docker Desktop",
		app:         appPath,
		cliPaths:    []string{cliPath},
		cliLinkHint: "add Docker Desktop CLI tools to PATH",
	}}
	t.Setenv("PATH", filepath.Join(dir, "no-docker-on-path"))
	cliDir := filepath.Dir(cliPath)

	dockerLookPathFn = func(file string) (string, error) {
		if file == "docker" && pathHasDir(os.Getenv("PATH"), cliDir) {
			return cliPath, nil
		}
		return "", errors.New("not found")
	}
	versionChecks := 0
	dockerVersionOKFn = func(context.Context) bool {
		versionChecks++
		return pathHasDir(os.Getenv("PATH"), cliDir)
	}

	if err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSWindows); err != nil {
		t.Fatalf("ensureDockerDaemonForHostOS: %v", err)
	}
	if !pathHasDir(os.Getenv("PATH"), cliDir) {
		t.Fatalf("PATH = %q, want bundled CLI dir %q", os.Getenv("PATH"), cliDir)
	}
	if versionChecks < 2 {
		t.Fatalf("version checks = %d, want initial failure and retry after PATH update", versionChecks)
	}
}

func TestEnsureDockerDaemon_WindowsRuntimeInstalledButDockerCLIMissingDiagnostic(t *testing.T) {
	oldWindowsRuntimes := windowsDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	t.Cleanup(func() {
		windowsDockerRuntimes = oldWindowsRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
	})

	dir := t.TempDir()
	appPath := filepath.Join(dir, "Docker Desktop.exe")
	if err := os.WriteFile(appPath, []byte("exe"), 0o755); err != nil {
		t.Fatal(err)
	}

	windowsDockerRuntimes = []dockerRuntime{{
		name:        "Docker Desktop",
		app:         appPath,
		cliPaths:    []string{filepath.Join(dir, "resources", "bin", "docker.exe")},
		cliLinkHint: "repair Docker Desktop CLI tools",
	}}
	t.Setenv("PATH", filepath.Join(dir, "no-docker-on-path"))
	dockerLookPathFn = func(string) (string, error) { return "", errors.New("not found") }
	dockerVersionOKFn = func(context.Context) bool { return false }

	err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSWindows)
	if err == nil {
		t.Fatal("expected docker CLI missing diagnostic")
	}
	msg := err.Error()
	for _, want := range []string{"Docker Desktop is installed", "docker CLI is not on PATH", "repair Docker Desktop CLI tools"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want substring %q", msg, want)
		}
	}
}

func TestDetectProjectType_Dockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "docker" {
		t.Errorf("detectProjectType = %q; want %q", got, "docker")
	}
}

func TestDetectProjectType_PackageSwift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "swift" {
		t.Errorf("detectProjectType = %q; want %q", got, "swift")
	}
}

func TestDetectProjectType_RequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("setup()"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_PyprojectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.poetry]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_Unknown(t *testing.T) {
	dir := t.TempDir()
	if got := mustDetectProjectType(t, dir); got != "unknown" {
		t.Errorf("detectProjectType = %q; want %q", got, "unknown")
	}
}

func TestDetectProjectType_DockerfileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	// Create both Dockerfile and requirements.txt; Dockerfile should win.
	for _, name := range []string{"Dockerfile", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDetectProjectType(t, dir); got != "docker" {
		t.Errorf("detectProjectType = %q; want %q (Dockerfile should take precedence)", got, "docker")
	}
}

func TestDetectProjectType_DockerfileVariantOnly(t *testing.T) {
	dir := t.TempDir()
	// No base Dockerfile — only variants.
	for _, name := range []string{"Dockerfile.dev", "Dockerfile.prod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM alpine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDetectProjectType(t, dir); got != "docker" {
		t.Errorf("detectProjectType = %q; want %q (Dockerfile variant should be recognised)", got, "docker")
	}
}

func TestDetectProjectType_XcodeOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "xcode" {
		t.Errorf("detectProjectType = %q; want %q", got, "xcode")
	}
}

func TestDetectProjectType_SwiftPMWinsOverXcode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "swift" {
		t.Errorf("detectProjectType = %q; want %q (Package.swift should take precedence)", got, "swift")
	}
}

func TestDetectProjectType_MultipleXcodeprojs_Error(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"First.xcodeproj", "Second.xcodeproj"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := detectProjectType(dir)
	if err == nil {
		t.Fatal("detectProjectType expected error for multiple .xcodeproj dirs, got nil")
	}
	if !strings.Contains(err.Error(), "multiple .xcodeproj") {
		t.Errorf("expected 'multiple .xcodeproj' in error, got: %v", err)
	}
}

func TestResolveDetectedBuildOption_PrefersDockerfileOverSwift(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got, err := resolveDetectedBuildOption(options, "", "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.Type != "docker" || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile docker option", got)
	}
}

func TestResolveDetectedBuildOption_PrefersDockerfileOverPython(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "requirements.txt (Python)", Type: "python", File: "requirements.txt"},
	}

	got, err := resolveDetectedBuildOption(options, "", "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.Type != "docker" || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile docker option", got)
	}
}

func TestPreferredBuildOption_InteractiveMultipleDockerfilesDoesNotAutoPrefer(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got := preferredBuildOption(options, true)
	if got != nil {
		t.Fatalf("got %+v, want nil so the picker can choose among Dockerfiles", got)
	}
}

func TestBuildOptionForType_DockerUsesExactDockerfile(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got, err := buildOptionForType(options, "docker", false)
	if err != nil {
		t.Fatalf("buildOptionForType: %v", err)
	}
	if got == nil || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile", got)
	}
}

// resolveDetectedBuildOption uses term.IsTerminal which returns false in test
// environments (stdin is a pipe). These tests therefore exercise the
// non-interactive code path.

func TestResolveDetectedBuildOption_NonInteractiveMultipleDockerfilesPrefersBase(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Dockerfile.prod", Type: "docker", File: "Dockerfile.prod"},
	}

	got, err := resolveDetectedBuildOption(options, "", "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want base Dockerfile", got)
	}
}

func TestResolveDetectedBuildOption_NonInteractiveVariantOnlyDockerfilesPrefersFirst(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Dockerfile.prod", Type: "docker", File: "Dockerfile.prod"},
	}

	got, err := resolveDetectedBuildOption(options, "", "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.File != "Dockerfile.dev" {
		t.Fatalf("got %+v, want Dockerfile.dev (first variant)", got)
	}
}

func TestResolveDetectedBuildOption_DockerfileFlag(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Dockerfile.prod", Type: "docker", File: "Dockerfile.prod"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got, err := resolveDetectedBuildOption(options, "", "Dockerfile.prod")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.File != "Dockerfile.prod" {
		t.Fatalf("got %+v, want Dockerfile.prod", got)
	}
}

func TestResolveDetectedBuildOption_DockerfileFlagNotFound(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
	}

	_, err := resolveDetectedBuildOption(options, "", "Dockerfile.missing")
	if err == nil {
		t.Fatal("expected error for missing dockerfile")
	}
}

func TestResolveRunDockerfile_SingleDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.prod"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRunDockerfile(dir, runOptions{}, false)
	if err != nil {
		t.Fatalf("resolveRunDockerfile: %v", err)
	}
	if got != "Dockerfile.prod" {
		t.Fatalf("got %q, want Dockerfile.prod", got)
	}
}

func TestResolveRunDockerfile_ExplicitFlag(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Dockerfile.prod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunDockerfile(dir, runOptions{dockerfile: "Dockerfile.prod"}, false)
	if err != nil {
		t.Fatalf("resolveRunDockerfile: %v", err)
	}
	if got != "Dockerfile.prod" {
		t.Fatalf("got %q, want Dockerfile.prod", got)
	}
}

func TestResolveRunDockerfile_MultipleNonInteractivePrefersBase(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Dockerfile.dev", "Dockerfile.prod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunDockerfile(dir, runOptions{}, false)
	if err != nil {
		t.Fatalf("resolveRunDockerfile: %v", err)
	}
	if got != "Dockerfile" {
		t.Fatalf("got %q, want Dockerfile", got)
	}
}

func TestResolveRunProjectType_DockerfileVariantOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.prod"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRunProjectType(dir, "docker")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "docker" {
		t.Fatalf("got %q, want docker", got)
	}
}

func TestResolveRunProjectType_DefaultPrefersDockerfile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Package.swift"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "docker" {
		t.Fatalf("got %q, want docker", got)
	}
}

func TestResolveRunProjectType_SwiftOverride(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Package.swift"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "swift")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "swift" {
		t.Fatalf("got %q, want swift", got)
	}
}

func TestResolveRegistryForAgentUsesConnectionDialer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var dialedPort int
	conn := &grpcclient.AgentConnection{
		Host: "cloud-device-name",
		RegistryDialer: func(_ context.Context, port int) (net.Conn, error) {
			dialedPort = port
			proxySide, registrySide := net.Pipe()
			go func() {
				defer registrySide.Close()
				buf := make([]byte, 16)
				n, err := registrySide.Read(buf)
				if err == nil && n > 0 {
					_, _ = registrySide.Write(buf[:n])
				}
			}()
			return proxySide, nil
		},
	}

	registryAddr, cleanup, err := resolveRegistryForAgent(ctx, conn, 5000)
	if err != nil {
		t.Fatalf("resolveRegistryForAgent: %v", err)
	}
	defer cleanup()

	_, port, err := net.SplitHostPort(registryAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", registryAddr, err)
	}
	tcpConn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer tcpConn.Close()

	if _, err := tcpConn.Write([]byte("ping")); err != nil {
		t.Fatalf("write proxy: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := tcpConn.Read(buf); err != nil {
		t.Fatalf("read proxy: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("proxy echoed %q, want ping", string(buf))
	}
	if dialedPort != 5000 {
		t.Fatalf("dialed port = %d, want 5000", dialedPort)
	}
}

func TestResolveRunProjectType_PythonOverride(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "python")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "python" {
		t.Fatalf("got %q, want python", got)
	}
}

func TestResolveRunProjectType_InvalidOverride(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveRunProjectType(dir, "ruby")
	if err == nil {
		t.Fatal("expected error for invalid run build type override")
	}
	if !strings.Contains(err.Error(), `invalid value "ruby" for --build-type`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRunProjectType_PropagatesMarkerStatErrors(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveRunProjectType(notDir, "docker")
	if err == nil {
		t.Fatal("expected stat error for invalid project path")
	}
	if !strings.Contains(err.Error(), "checking for") {
		t.Fatalf("expected wrapped stat error, got %v", err)
	}
}

func TestGeneratePythonDockerfile_WithRequirements(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	expectations := []string{
		"FROM python:3.11-slim",
		"WORKDIR /app",
		"COPY requirements.txt .",
		"RUN pip install --no-cache-dir -r requirements.txt",
		"COPY . .",
		`CMD ["python", "app.py"]`,
	}
	for _, exp := range expectations {
		if !strings.Contains(content, exp) {
			t.Errorf("generated Dockerfile missing %q\nGot:\n%s", exp, content)
		}
	}
}

func TestGeneratePythonDockerfile_WithoutRequirements_MainPy(t *testing.T) {
	dir := t.TempDir()
	// Only main.py, no requirements.txt, no app.py.
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "requirements.txt") {
		t.Error("Dockerfile should not mention requirements.txt when it does not exist")
	}
	if !strings.Contains(content, `CMD ["python", "main.py"]`) {
		t.Errorf("expected CMD with main.py, got:\n%s", content)
	}
}

func TestGeneratePythonDockerfile_FallbackEntrypoint(t *testing.T) {
	dir := t.TempDir()
	// No app.py or main.py; should fall back to app.py as default.

	_, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `CMD ["python", "app.py"]`) {
		t.Errorf("expected fallback to app.py entrypoint, got:\n%s", string(data))
	}
}

func TestRegistryHost_IPv4(t *testing.T) {
	got := registryHost("192.168.1.5", 5000)
	if got != "192.168.1.5:5000" {
		t.Errorf("registryHost IPv4 = %q, want %q", got, "192.168.1.5:5000")
	}
}

func TestRegistryHost_IPv6Global(t *testing.T) {
	got := registryHost("2001:db8::1", 5000)
	if got != "[2001:db8::1]:5000" {
		t.Errorf("registryHost IPv6 global = %q, want %q", got, "[2001:db8::1]:5000")
	}
}

func TestRegistryHost_IPv6LinkLocalWithZone(t *testing.T) {
	got := registryHost("fe80::2ecf:67ff:feba:6cca%en0", 5000)
	// Zone ID must be stripped — it's host-specific and unusable in containers.
	if got != "[fe80::2ecf:67ff:feba:6cca]:5000" {
		t.Errorf("registryHost IPv6 link-local+zone = %q, want %q", got, "[fe80::2ecf:67ff:feba:6cca]:5000")
	}
}

func TestRegistryHost_IPv6LinkLocalNoZone(t *testing.T) {
	got := registryHost("fe80::1", 5000)
	if got != "[fe80::1]:5000" {
		t.Errorf("registryHost IPv6 link-local no zone = %q, want %q", got, "[fe80::1]:5000")
	}
}

func TestSplitIPv6RegistryAddr_IPv6WithZone(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("[fe80::1%en0]:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want %q", eff, "wendy-registry:5000")
	}
	if ip != "fe80::1" {
		t.Errorf("ipv6IP = %q, want %q (zone stripped)", ip, "fe80::1")
	}
}

func TestSplitIPv6RegistryAddr_IPv6NoZone(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("[2001:db8::1]:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want %q", eff, "wendy-registry:5000")
	}
	if ip != "2001:db8::1" {
		t.Errorf("ipv6IP = %q, want %q", ip, "2001:db8::1")
	}
}

func TestSplitIPv6RegistryAddr_IPv4Passthrough(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("192.168.1.5:5000")
	if eff != "192.168.1.5:5000" {
		t.Errorf("effectiveAddr = %q, want unchanged", eff)
	}
	if ip != "" {
		t.Errorf("ipv6IP = %q, want empty for IPv4", ip)
	}
}

func TestSplitIPv6RegistryAddr_HostnamePassthrough(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("wendy-registry:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want unchanged", eff)
	}
	if ip != "" {
		t.Errorf("ipv6IP = %q, want empty for hostname", ip)
	}
}

func TestResolveRegistryIP_StripZone(t *testing.T) {
	got := resolveRegistryIP("fe80::1%eth0")
	if got != "fe80::1" {
		t.Errorf("resolveRegistryIP zone = %q, want %q", got, "fe80::1")
	}
}

func TestResolveRegistryIP_IPv4Passthrough(t *testing.T) {
	got := resolveRegistryIP("10.0.0.1")
	if got != "10.0.0.1" {
		t.Errorf("resolveRegistryIP IPv4 = %q, want %q", got, "10.0.0.1")
	}
}

func TestIsLinkLocalIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"169.254.1.1", true},
		{"169.254.189.250", true},
		{"192.168.1.5", false},
		{"10.0.0.1", false},
		{"fe80::1", true},
		{"[fe80::1]", true},
		{"2001:db8::1", false},
		{"not-an-ip", false},
	}
	for _, tt := range tests {
		if got := isLinkLocalIP(tt.ip); got != tt.want {
			t.Errorf("isLinkLocalIP(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestStartRegistryProxy(t *testing.T) {
	// Start a fake "registry" server.
	fakeRegistry := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		fakeRegistry <- string(buf[:n])
		conn.Write([]byte("OK"))
	}()

	// Start the proxy pointing at the fake registry.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy, err := startRegistryProxy(ctx, "127.0.0.1:0", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	// Connect through the proxy.
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(proxy.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte("PUSH"))
	got := <-fakeRegistry
	if got != "PUSH" {
		t.Errorf("proxy forwarded %q, want %q", got, "PUSH")
	}
}

func TestFindIPv4ViaNeighborTable_UnknownAddress(t *testing.T) {
	// This test would invoke findIPv4ViaNeighborTable, which may spawn real ndp/arp/ip
	// commands and read the host's neighbor tables, making it environment-dependent.
	// Skip it to avoid flakiness/timeouts in unit test environments.
	t.Skip("disabled: findIPv4ViaNeighborTable depends on host neighbor tables and OS commands")
}

// testCert holds a certificate and its signing key for use in TLS test setups.
type testCert struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pemStr string
}

// generateTestCA creates a self-signed CA certificate.
func generateTestCA(t *testing.T) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-root-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCert{
		cert:   cert,
		key:    key,
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// generateTestIntermediate creates an intermediate CA signed by the given parent CA.
func generateTestIntermediate(t *testing.T, parent *testCert) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate intermediate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "test-intermediate-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent.cert, &key.PublicKey, parent.key)
	if err != nil {
		t.Fatalf("create intermediate cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}
	return &testCert{
		cert:   cert,
		key:    key,
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// generateTestLeafNoSAN creates a server-auth leaf certificate with no SANs,
// simulating a device cert that lacks a hostname or IP SAN (the common Wendy case).
func generateTestLeafNoSAN(t *testing.T, issuer *testCert) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: "test-leaf-no-san"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Deliberately no IPAddresses or DNSNames.
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.cert, &key.PublicKey, issuer.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return &testCert{
		cert:   cert,
		key:    key,
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// generateTestLeaf creates a leaf certificate signed by the given issuer.
// eku controls the ExtKeyUsage (use x509.ExtKeyUsageServerAuth or x509.ExtKeyUsageClientAuth).
// IPAddresses is populated only for server certs (ServerAuth) so hostname verification works.
func generateTestLeaf(t *testing.T, issuer *testCert, eku x509.ExtKeyUsage) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	if eku == x509.ExtKeyUsageServerAuth {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.cert, &key.PublicKey, issuer.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return &testCert{
		cert:   cert,
		key:    key,
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// marshalKeyPEM encodes an ECDSA private key to PEM.
func marshalKeyPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// startTestTLSServer starts a TLS HTTPS server that responds with 200 OK.
// tlsCert configures the server's certificate chain (leaf + optional intermediates).
// clientCA, if non-nil, enables mutual TLS and requires client certs signed by it.
func startTestTLSServer(t *testing.T, tlsCert tls.Certificate, clientCA *testCert) string {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	if clientCA != nil {
		pool := x509.NewCertPool()
		pool.AddCert(clientCA.cert)
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestStartMTLSRegistryHTTPProxy_DirectCA(t *testing.T) {
	ca := generateTestCA(t)
	serverLeaf := generateTestLeaf(t, ca, x509.ExtKeyUsageServerAuth)
	clientLeaf := generateTestLeaf(t, ca, x509.ExtKeyUsageClientAuth)

	serverTLSCert, err := tls.X509KeyPair([]byte(serverLeaf.pemStr), []byte(marshalKeyPEM(t, serverLeaf.key)))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	addr := startTestTLSServer(t, serverTLSCert, ca)

	proxy, err := startMTLSRegistryHTTPProxy(addr, clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), ca.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()

	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStartMTLSRegistryHTTPProxy_IntermediateChain(t *testing.T) {
	root := generateTestCA(t)
	intermediate := generateTestIntermediate(t, root)
	serverLeaf := generateTestLeaf(t, intermediate, x509.ExtKeyUsageServerAuth)
	clientLeaf := generateTestLeaf(t, root, x509.ExtKeyUsageClientAuth)

	// Server sends leaf + intermediate in the TLS handshake.
	serverTLSCert := tls.Certificate{
		Certificate: [][]byte{serverLeaf.cert.Raw, intermediate.cert.Raw},
		PrivateKey:  serverLeaf.key,
	}
	addr := startTestTLSServer(t, serverTLSCert, root)

	proxy, err := startMTLSRegistryHTTPProxy(addr, clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), root.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()

	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStartMTLSRegistryHTTPProxy_WrongCA(t *testing.T) {
	trustedCA := generateTestCA(t)
	untrustedCA := generateTestCA(t)
	serverLeaf := generateTestLeaf(t, untrustedCA, x509.ExtKeyUsageServerAuth)
	clientLeaf := generateTestLeaf(t, trustedCA, x509.ExtKeyUsageClientAuth)

	serverTLSCert, err := tls.X509KeyPair([]byte(serverLeaf.pemStr), []byte(marshalKeyPEM(t, serverLeaf.key)))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	// Don't require client certs on the server side so we test the proxy's cert verification.
	addr := startTestTLSServer(t, serverTLSCert, nil)

	proxy, err := startMTLSRegistryHTTPProxy(addr, clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), trustedCA.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()

	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		// Transport-level TLS rejection surfaces as a connection error via the proxy.
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected non-200 when server cert is signed by untrusted CA, got 200")
	}
}

func TestStartMTLSRegistryHTTPProxy_NoSAN(t *testing.T) {
	// Device certs signed by the Wendy CA often lack a SAN for the target
	// mDNS hostname. Verify the proxy accepts such certs via chain validation
	// (VerifyConnection) while InsecureSkipVerify bypasses hostname checks.
	ca := generateTestCA(t)
	serverLeaf := generateTestLeafNoSAN(t, ca)
	clientLeaf := generateTestLeaf(t, ca, x509.ExtKeyUsageClientAuth)

	serverTLSCert, err := tls.X509KeyPair([]byte(serverLeaf.pemStr), []byte(marshalKeyPEM(t, serverLeaf.key)))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	addr := startTestTLSServer(t, serverTLSCert, ca)

	proxy, err := startMTLSRegistryHTTPProxy(addr, clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), ca.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()

	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (cert without SAN should be accepted via chain validation)", resp.StatusCode)
	}
}

func TestStartMTLSRegistryHTTPProxy_UntrustedClientCert(t *testing.T) {
	// The server requires a client cert from trustedCA, but the proxy presents
	// one from untrustedCA. The server should reject the connection.
	trustedCA := generateTestCA(t)
	untrustedCA := generateTestCA(t)
	serverLeaf := generateTestLeaf(t, trustedCA, x509.ExtKeyUsageServerAuth)
	clientLeaf := generateTestLeaf(t, untrustedCA, x509.ExtKeyUsageClientAuth)

	serverTLSCert, err := tls.X509KeyPair([]byte(serverLeaf.pemStr), []byte(marshalKeyPEM(t, serverLeaf.key)))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	// Server requires client certs signed by trustedCA.
	addr := startTestTLSServer(t, serverTLSCert, trustedCA)

	proxy, err := startMTLSRegistryHTTPProxy(addr, clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), trustedCA.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()

	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		// Transport-level rejection from the server is acceptable.
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected non-200 when proxy presents a client cert from an untrusted CA, got 200")
	}
}

func TestResolveDockerfile_NoDockerfiles(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveDockerfile(dir, "", false)
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveDockerfile_RequestedPassthrough(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Dockerfile.prod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveDockerfile(dir, "Dockerfile.prod", false)
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got != "Dockerfile.prod" {
		t.Fatalf("got %q, want Dockerfile.prod", got)
	}
}

func TestResolveDockerfile_SingleVariant(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.dev"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDockerfile(dir, "", false)
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got != "Dockerfile.dev" {
		t.Fatalf("got %q, want Dockerfile.dev", got)
	}
}

func TestResolveDockerfile_MultipleNonInteractivePrefersBase(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Dockerfile.prod", "Dockerfile.dev"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveDockerfile(dir, "", false)
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got != "Dockerfile" {
		t.Fatalf("got %q, want Dockerfile", got)
	}
}

func TestResolveDockerfile_MultipleNonInteractiveVariantOnlyPrefersFirst(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile.dev", "Dockerfile.prod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveDockerfile(dir, "", false)
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if got == "" {
		t.Fatal("got empty, want a Dockerfile variant")
	}
}

func TestValidateDockerfileName(t *testing.T) {
	valid := []string{"Dockerfile", "Dockerfile.prod", "Dockerfile.dev", "Dockerfile-prod", "Dockerfile.my.variant"}
	for _, name := range valid {
		if err := validateDockerfileName(name); err != nil {
			t.Errorf("validateDockerfileName(%q) unexpected error: %v", name, err)
		}
	}
	invalid := []string{"-flag", "dockerfile", "DOCKERFILE", "not-a-dockerfile", "Dockerfile/evil", ".hidden", "Dockerfile.dockerignore", "Dockerfile.prod.dockerignore"}
	for _, name := range invalid {
		if err := validateDockerfileName(name); err == nil {
			t.Errorf("validateDockerfileName(%q) expected error, got nil", name)
		}
	}
}

func TestConfinedDockerfilePath_Traversal(t *testing.T) {
	dir := t.TempDir()
	if _, err := confinedDockerfilePath(dir, "../../etc/passwd"); err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}

func TestConfinedDockerfilePath_NotExist(t *testing.T) {
	dir := t.TempDir()
	if _, err := confinedDockerfilePath(dir, "Dockerfile.notexist"); err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestConfinedDockerfilePath_Valid(t *testing.T) {
	dir := t.TempDir()
	name := "Dockerfile.prod"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := confinedDockerfilePath(dir, name)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty resolved path")
	}
}

func TestConfinedDockerfilePath_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	// Create a Dockerfile in the target (outside dir).
	if err := os.WriteFile(filepath.Join(target, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink inside dir pointing to target dir.
	linkPath := filepath.Join(dir, "link")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	if _, err := confinedDockerfilePath(dir, "link/Dockerfile"); err == nil {
		t.Fatal("expected error for symlink escape, got nil")
	}
}

// TestResolveDockerfile_AutoSelectionRejectsSymlinkEscape verifies that the
// auto-selection path (no explicit --dockerfile) applies confinement checks,
// so a Dockerfile symlink pointing outside the project is rejected.
func TestResolveDockerfile_AutoSelectionRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// Create the real Dockerfile outside the project.
	if err := os.WriteFile(filepath.Join(outside, "contents"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Place a Dockerfile.prod symlink inside the project pointing outside.
	link := filepath.Join(dir, "Dockerfile.prod")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	if _, err := resolveDockerfile(dir, "", false); err == nil {
		t.Fatal("expected error for auto-selected symlink escape, got nil")
	}
}
