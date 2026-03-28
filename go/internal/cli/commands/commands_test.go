package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestResolveRestartPolicy_Default(t *testing.T) {
	opts := runOptions{}
	rp := resolveRestartPolicy(opts)
	if rp == nil {
		t.Fatal("resolveRestartPolicy returned nil")
	}
	if rp.Mode != agentpb.RestartPolicyMode_DEFAULT {
		t.Errorf("Mode = %v; want DEFAULT", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStopped(t *testing.T) {
	opts := runOptions{restartUnlessStopped: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED", rp.Mode)
	}
}

func TestResolveRestartPolicy_OnFailure(t *testing.T) {
	opts := runOptions{restartOnFailure: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_ON_FAILURE {
		t.Errorf("Mode = %v; want ON_FAILURE", rp.Mode)
	}
}

func TestResolveRestartPolicy_NoRestart(t *testing.T) {
	opts := runOptions{noRestart: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_NO {
		t.Errorf("Mode = %v; want NO", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStoppedTakesPrecedence(t *testing.T) {
	// When multiple flags are set, restartUnlessStopped should win (checked first).
	opts := runOptions{
		restartUnlessStopped: true,
		restartOnFailure:     true,
		noRestart:            true,
	}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED (should take precedence)", rp.Mode)
	}
}

func TestNewRunCmd(t *testing.T) {
	cmd := newRunCmd()
	if cmd.Use != "run" {
		t.Errorf("Use = %q; want %q", cmd.Use, "run")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify expected flags exist.
	expectedFlags := []string{"debug", "deploy", "detach", "restart-unless-stopped", "restart-on-failure", "no-restart", "prefix", "user-args"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

func TestResolveRunWorkingDir_Default(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	got, err := resolveRunWorkingDir(runOptions{})
	if err != nil {
		t.Fatalf("resolveRunWorkingDir: %v", err)
	}
	if canonicalPath(t, got) != canonicalPath(t, tempDir) {
		t.Fatalf("got %q, want %q", got, tempDir)
	}
}

func TestResolveRunWorkingDir_RelativePrefix(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	projectDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	got, err := resolveRunWorkingDir(runOptions{prefix: filepath.Join("apps", "demo")})
	if err != nil {
		t.Fatalf("resolveRunWorkingDir: %v", err)
	}
	if canonicalPath(t, got) != canonicalPath(t, projectDir) {
		t.Fatalf("got %q, want %q", got, projectDir)
	}
}

func TestResolveRunWorkingDir_MissingPrefix(t *testing.T) {
	_, err := resolveRunWorkingDir(runOptions{prefix: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "does not exist") {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestResolveRunWorkingDir_NotDirectory(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "wendy.json")
	if err := os.WriteFile(filePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := resolveRunWorkingDir(runOptions{prefix: filePath})
	if err == nil {
		t.Fatal("expected error for file path")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "not a directory") {
		t.Fatalf("unexpected error: %q", got)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	return filepath.Clean(path)
}

func TestNewBuildCmd(t *testing.T) {
	cmd := newBuildCmd()
	if cmd.Use != "build" {
		t.Errorf("Use = %q; want %q", cmd.Use, "build")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}
}

func TestNewDeviceCmd(t *testing.T) {
	cmd := newDeviceCmd()
	if cmd.Use != "device" {
		t.Errorf("Use = %q; want %q", cmd.Use, "device")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify subcommands exist.
	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"version", "set-default", "unset-default", "setup", "update"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("device command missing subcommand %q", name)
		}
	}
}

func TestNewAuthCmd(t *testing.T) {
	cmd := newAuthCmd()
	if cmd.Use != "auth" {
		t.Errorf("Use = %q; want %q", cmd.Use, "auth")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"login", "logout", "refresh-certs"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("auth command missing subcommand %q", name)
		}
	}
}

func makeELFHeader(machine uint16) []byte {
	hdr := make([]byte, 20)
	hdr[0], hdr[1], hdr[2], hdr[3] = 0x7f, 'E', 'L', 'F'
	hdr[4] = 2 // 64-bit
	hdr[5] = 1 // little-endian
	hdr[18] = byte(machine)
	hdr[19] = byte(machine >> 8)
	return hdr
}

func TestCheckELFArchitecture(t *testing.T) {
	amd64ELF := makeELFHeader(62)
	arm64ELF := makeELFHeader(183)
	notELF := []byte("#!/bin/sh\necho hi\n")

	cases := []struct {
		name       string
		data       []byte
		deviceArch string
		wantErr    bool
	}{
		{"amd64 binary on amd64 device", amd64ELF, "amd64", false},
		{"arm64 binary on arm64 device", arm64ELF, "arm64", false},
		{"amd64 binary on arm64 device", amd64ELF, "arm64", true},
		{"arm64 binary on amd64 device", arm64ELF, "amd64", true},
		{"non-ELF accepted on any device", notELF, "arm64", false},
		{"too short data accepted", []byte{0x7f, 'E'}, "amd64", false},
		{"unsupported device arch rejected", amd64ELF, "riscv64", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkELFArchitecture(tc.data, tc.deviceArch)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
