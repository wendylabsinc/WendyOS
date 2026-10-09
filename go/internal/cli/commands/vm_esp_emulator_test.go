package commands

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestESPEmulatorInstallation(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch, existing, wantError           string
		interactive, yes, json, accept, fail, missing bool
		wantPrompt, wantInstall                       bool
	}{
		{name: "PATH", os: "darwin", arch: "arm64", existing: "path"},
		{name: "managed", os: "linux", arch: "amd64", existing: "managed"},
		{name: "macOS", os: "darwin", arch: "arm64", interactive: true, accept: true, wantPrompt: true, wantInstall: true},
		{name: "Linux x86", os: "linux", arch: "amd64", interactive: true, accept: true, wantPrompt: true, wantInstall: true},
		{name: "Linux ARM", os: "linux", arch: "arm64", yes: true, wantInstall: true},
		{name: "decline", os: "darwin", arch: "arm64", interactive: true, wantPrompt: true, wantError: "cancel"},
		{name: "headless", os: "linux", arch: "amd64", wantError: "curl -fsSL"},
		{name: "JSON", os: "linux", arch: "amd64", interactive: true, json: true, yes: true, wantError: "curl -fsSL"},
		{name: "Windows", os: "windows", arch: "amd64", interactive: true, wantError: "WSL2"},
		{name: "Intel Mac", os: "darwin", arch: "amd64", interactive: true, wantError: "no prebuilt release"},
		{name: "ARM32", os: "linux", arch: "arm", interactive: true, wantError: "no prebuilt release"},
		{name: "install fails", os: "linux", arch: "amd64", yes: true, fail: true, wantInstall: true, wantError: "download failed"},
		{name: "missing binary", os: "linux", arch: "amd64", yes: true, missing: true, wantInstall: true, wantError: "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			look, install, dir, interactive, confirm := espEmulatorLookPath, installESPEmulatorFn, espEmulatorDir, isInteractiveTerminalFn, confirmFn
			yes, json := vmAssumeYes, jsonOutput
			t.Cleanup(func() {
				espEmulatorLookPath, installESPEmulatorFn, espEmulatorDir, isInteractiveTerminalFn, confirmFn = look, install, dir, interactive, confirm
				vmAssumeYes, jsonOutput = yes, json
			})
			root := t.TempDir()
			managed := filepath.Join(root, "esp-emu")
			installed, prompted := false, false
			espEmulatorDir = func() (string, error) { return root, nil }
			espEmulatorLookPath = func(s string) (string, error) {
				if s == "esp-emu" && tc.existing == "path" {
					return "/usr/local/bin/esp-emu", nil
				}
				if s == managed && (tc.existing == "managed" || (installed && !tc.missing)) {
					return managed, nil
				}
				return "", exec.ErrNotFound
			}
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			vmAssumeYes, jsonOutput = tc.yes, tc.json
			confirmFn = func(q string) bool {
				prompted = true
				if q != "Do you want to install the ESP32 Emulator?" {
					t.Fatalf("prompt: %q", q)
				}
				return tc.accept
			}
			installESPEmulatorFn = func(context.Context, string) error {
				installed = true
				if tc.fail {
					return errors.New("download failed")
				}
				return nil
			}
			path, err := ensureESPEmulatorForHost(context.Background(), tc.os, tc.arch)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantError)) {
					t.Fatalf("error: %v", err)
				}
			} else if err != nil || path == "" {
				t.Fatalf("path=%q error=%v", path, err)
			}
			if prompted != tc.wantPrompt || installed != tc.wantInstall {
				t.Fatalf("prompted=%v installed=%v", prompted, installed)
			}
		})
	}
}
