package appsupport

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCaptureConfinesSymlinksAndRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"escape", "pipe", "."} {
		if f, err := OpenCapture(root, name); err == nil {
			f.Close()
			t.Fatalf("accepted %s", name)
		}
	}
	f, err := OpenCapture(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
