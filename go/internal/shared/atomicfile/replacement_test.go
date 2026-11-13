package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A failed repair must leave usable state behind, including when the caller
// retries later. Exercise the public writers against a real existing file.
func TestFailedReplacementPreservesStateAndCanRetry(t *testing.T) {
	for name, writer := range map[string]func(string, []byte, os.FileMode) error{
		"agent state":                Write,
		"CLI state preserving owner": WritePreservingOwner,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "provisioning.json")
			old := `{"device":"enrolled","key":"original"}`
			next := `{"device":"enrolled","key":"renewed"}`
			if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			refused := errors.New("injected permanent rename failure")
			stubHost(t, runtime.GOOS, func(string, string) error { return refused })
			if err := writer(path, []byte(next), 0o600); !errors.Is(err, refused) {
				t.Fatalf("write error = %v, want original rename cause", err)
			}
			check := func(want string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("persisted state = %q, %v; want %q", got, err, want)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
					t.Fatalf("replacement left temporary files: %v, %v", entries, err)
				}
			}
			check(old)
			renameFn = os.Rename
			if err := writer(path, []byte(next), 0o600); err != nil {
				t.Fatalf("retry: %v", err)
			}
			check(next)
		})
	}
}
