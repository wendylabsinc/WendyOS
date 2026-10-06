//go:build !windows

package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Off Windows a refused rename is final, even one whose errno happens to share
// its number with a retryable Windows error: the write fails after a single
// rename, with no sleep, exactly as before the Windows retry existed.
func TestWriteNeverRetriesARenameOffWindows(t *testing.T) {
	prevSleep := sleepFn
	sleepFn = func(d time.Duration) { t.Errorf("slept %v before retrying a rename off Windows", d) }
	t.Cleanup(func() { sleepFn = prevSleep })
	// ERROR_FILE_NOT_FOUND, ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION.
	for _, errno := range []syscall.Errno{2, 5, 32} {
		calls := 0
		prevRename := renameFn
		renameFn = func(from, to string) error {
			calls++
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errno}
		}
		if err := Write(filepath.Join(t.TempDir(), "config.json"), []byte("x"), 0o600); err == nil {
			t.Errorf("errno %d: Write succeeded although the rename failed", errno)
		}
		renameFn = prevRename
		if calls != 1 {
			t.Errorf("errno %d: rename called %d times off Windows, want exactly 1", errno, calls)
		}
	}
}
