package vm

import (
	"path/filepath"
	"testing"
)

func TestSpecUsesWindowsQEMUPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qemu-system-aarch64.exe")
	saved := vmLookPath
	t.Cleanup(func() { vmLookPath = saved })
	vmLookPath = func(string) (string, error) { return path, nil }
	if got := (Spec{}).Binary(); got != path {
		t.Fatalf("launch binary = %q, want resolved Windows installation %q", got, path)
	}
}
