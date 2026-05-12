//go:build !windows

package swifttoolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnzipOverwriteEnv_UnixWritesShim(t *testing.T) {
	env, cleanup, err := unzipOverwriteEnv()
	if err != nil {
		t.Fatalf("unzipOverwriteEnv() error = %v, want nil", err)
	}
	if cleanup == nil {
		t.Fatal("unzipOverwriteEnv() cleanup = nil, want non-nil")
	}
	defer cleanup()

	// PATH must be prepended with a directory containing an executable shim.
	// Last PATH entry wins for child processes — we appended ours at the end.
	var pathVal string
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathVal = strings.TrimPrefix(e, "PATH=")
		}
	}
	if pathVal == "" {
		t.Fatal("env did not contain a PATH entry")
	}
	shimDir := strings.SplitN(pathVal, string(os.PathListSeparator), 2)[0]
	shimPath := filepath.Join(shimDir, "unzip")
	info, err := os.Stat(shimPath)
	if err != nil {
		t.Fatalf("expected unzip shim at %s: %v", shimPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("unzip shim is not executable: mode=%v", info.Mode())
	}

	contents, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", shimPath, err)
	}
	if !strings.Contains(string(contents), "unzip -o") {
		t.Errorf("shim content missing -o overwrite flag: %q", contents)
	}
}

func TestUnzipOverwriteEnv_UnixCleanupRemovesDir(t *testing.T) {
	env, cleanup, err := unzipOverwriteEnv()
	if err != nil {
		t.Fatalf("unzipOverwriteEnv() error = %v, want nil", err)
	}

	// Last PATH entry wins for child processes — we appended ours at the end.
	var pathVal string
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathVal = strings.TrimPrefix(e, "PATH=")
		}
	}
	shimDir := strings.SplitN(pathVal, string(os.PathListSeparator), 2)[0]

	cleanup()

	if _, err := os.Stat(shimDir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove shim dir %s (err=%v)", shimDir, err)
	}
}
