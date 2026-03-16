package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectProjectType_Dockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "docker" {
		t.Errorf("detectProjectType = %q; want %q", got, "docker")
	}
}

func TestDetectProjectType_PackageSwift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "swift" {
		t.Errorf("detectProjectType = %q; want %q", got, "swift")
	}
}

func TestDetectProjectType_RequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("setup()"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_PyprojectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.poetry]"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_Unknown(t *testing.T) {
	dir := t.TempDir()
	got := detectProjectType(dir)
	if got != "unknown" {
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
	got := detectProjectType(dir)
	if got != "docker" {
		t.Errorf("detectProjectType = %q; want %q (Dockerfile should take precedence)", got, "docker")
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

func TestEnsureSwiftVersion_AlreadyInstalled(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "true")
	}

	if err := ensureSwiftVersion(context.Background()); err != nil {
		t.Fatalf("ensureSwiftVersion() unexpected error: %v", err)
	}

	// When "swiftly which" succeeds, no install should happen.
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (which), got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "swiftly" || calls[0][1] != "which" || calls[0][2] != defaultSwiftVersion {
		t.Errorf("expected [swiftly which %s], got %v", defaultSwiftVersion, calls[0])
	}
}

func TestEnsureSwiftVersion_InstallsWhenMissing(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		// "swiftly which" fails (version not installed), "swiftly install" succeeds.
		if len(args) > 0 && args[0] == "which" {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}

	if err := ensureSwiftVersion(context.Background()); err != nil {
		t.Fatalf("ensureSwiftVersion() unexpected error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (which + install), got %d: %v", len(calls), calls)
	}
	if calls[0][1] != "which" {
		t.Errorf("first call should be which, got %v", calls[0])
	}
	if calls[1][1] != "install" || calls[1][2] != defaultSwiftVersion {
		t.Errorf("expected [swiftly install %s], got %v", defaultSwiftVersion, calls[1])
	}
}

func TestEnsureSwiftVersion_SwiftlyNotFound(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	}

	err := ensureSwiftVersion(context.Background())
	if err == nil {
		t.Fatal("ensureSwiftVersion() expected error when swiftly not found, got nil")
	}
	if !strings.Contains(err.Error(), "swiftly is required but not installed") {
		t.Errorf("expected actionable error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_InstallFails(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}

	err := ensureSwiftVersion(context.Background())
	if err == nil {
		t.Fatal("ensureSwiftVersion() expected error on install failure, got nil")
	}
	if !strings.Contains(err.Error(), "installing Swift") {
		t.Errorf("expected install error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_Cancellation(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ensureSwiftVersion(ctx)
	if err == nil {
		t.Fatal("ensureSwiftVersion() expected error on cancelled context, got nil")
	}
}
