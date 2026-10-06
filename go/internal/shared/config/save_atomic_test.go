package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A config left group-readable by an older tool or a manual copy is replaced by
// a private one: Save writes a fresh 0600 file and renames it into place, so a
// crash or a concurrent reader sees the old config or the new one, never a
// truncated file.
func TestSaveReplacesTheFileWithAPrivateOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"defaultDevice":"old.local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(&Config{DefaultDevice: "new.local"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("config.json mode = %v after Save, want 0600", got)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("Save left %q behind in the config dir", e.Name())
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "new.local" {
		t.Errorf("DefaultDevice = %q, want new.local", cfg.DefaultDevice)
	}
}

// Review Focus 2: people keep config.json in a dotfiles repo behind a symlink.
// A plain rename would silently replace the link with a regular file.
func TestSaveWritesThroughASymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	target := filepath.Join(t.TempDir(), "dotfiles-wendy-config.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if err := Save(&Config{DefaultDevice: "through-link.local"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Save replaced the config.json symlink with a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "through-link.local") {
		t.Fatalf("symlink target was not updated: %s", data)
	}
}

// A dangling symlink — its target does not exist yet, e.g. a freshly cloned
// dotfiles repo whose submodule hasn't been checked out — must still be
// written through, exactly like TestSaveWritesThroughASymlinkedConfig, and
// exactly like the old os.WriteFile (which follows a symlink to a missing
// target and creates it). filepath.EvalSymlinks fails outright on a dangling
// link, so writeConfigFile cannot just fall back to EvalSymlinks failing:
// falling back to the link path itself would replace the link with a
// regular file via rename instead of creating the target.
func TestSaveWritesThroughADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	target := filepath.Join(t.TempDir(), "dotfiles-wendy-config.json")
	// target intentionally does not exist: the symlink is dangling.
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if err := Save(&Config{DefaultDevice: "dangling-link.local"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Save replaced the dangling config.json symlink with a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target was not created: %v", err)
	}
	if !strings.Contains(string(data), "dangling-link.local") {
		t.Fatalf("symlink target was not written with the new content: %s", data)
	}
}

// Human decision F3: a read-only config.json (e.g. chmod 444) is the user's
// deliberate choice. A plain rename would silently replace it regardless of
// its permissions, so Save must fail exactly as the old os.WriteFile did, and
// must leave the file untouched.
func TestSaveRefusesToReplaceAReadOnlyConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file mode bits")
	}
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"defaultDevice":"readonly.local"}`)
	if err := os.WriteFile(path, original, 0o444); err != nil {
		t.Fatal(err)
	}

	err := Save(&Config{DefaultDevice: "new.local"})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Save = %v, want an error wrapping fs.ErrPermission", err)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(original) {
		t.Errorf("config.json content changed: got %s, want %s", data, original)
	}

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o444 {
			t.Errorf("config.json mode = %v after failed Save, want 0444", got)
		}
	}
}
