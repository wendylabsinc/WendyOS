//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePreservingOwnerDoesNotUseUnixOwnershipOnWindows(t *testing.T) {
	stubOwner(t, 0, func(string, int, int) error { t.Fatal("Windows replacement attempted chown"); return nil })
	path := filepath.Join(t.TempDir(), "config.json")
	for _, want := range []string{"original", "replacement"} {
		if err := WritePreservingOwner(path, []byte(want), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("state = %q, %v; want %q", got, err, want)
		}
	}
}
