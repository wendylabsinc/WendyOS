package pluginmode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFile creates path (and its parents) with content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestClient(t *testing.T) {
	for _, tt := range []struct{ env, want string }{
		{"", ""},
		{"claude", "claude"},
		{"codex", "codex"},
		{"CLAUDE", ""},
		{"cursor", ""},
		{" claude", ""},
	} {
		t.Setenv(EnvVar, tt.env)
		if got := Client(); got != tt.want {
			t.Errorf("WENDY_PLUGIN=%q: Client() = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestManagedInstall(t *testing.T) {
	root := t.TempDir()
	versioned := filepath.Join(root, "cli", "2026.10.01-120000", "wendy")
	pointer := filepath.Join(root, "bin", "wendy")
	elsewhere := filepath.Join(root, "other", "wendy")
	for _, p := range []string{versioned, pointer, elsewhere} {
		writeFile(t, p, "#!/bin/sh\n")
	}
	tests := []struct {
		name           string
		exe, configDir string
		want           bool
	}{
		{"versioned install", versioned, root, true},
		{"bin pointer (a copy on Windows)", pointer, root, true},
		{"another directory under the config dir", elsewhere, root, false},
		{"Homebrew", "/opt/homebrew/bin/wendy", root, false},
		{"the cli directory itself", filepath.Join(root, "cli"), root, false},
		{"empty exe", "", root, false},
		{"empty config dir", versioned, "", false},
	}
	for _, tt := range tests {
		if got := ManagedInstall(tt.exe, tt.configDir); got != tt.want {
			t.Errorf("%s: ManagedInstall(%q, %q) = %v, want %v", tt.name, tt.exe, tt.configDir, got, tt.want)
		}
	}
}

// macOS temp dirs live behind the /var -> /private/var symlink, and users
// symlink HOME or ~/.wendy; both sides are resolved before comparing.
func TestManagedInstall_SymlinkedConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	exe := filepath.Join(root, "cli", "2026.10.01-120000", "wendy")
	writeFile(t, exe, "#!/bin/sh\n")
	link := filepath.Join(t.TempDir(), "wendy-home")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if !ManagedInstall(exe, link) {
		t.Error("exe under the real dir must match a symlinked config dir")
	}
	if !ManagedInstall(filepath.Join(link, "cli", "2026.10.01-120000", "wendy"), root) {
		t.Error("exe reached through the symlink must match the real config dir")
	}
}
