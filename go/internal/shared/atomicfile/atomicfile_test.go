package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWriteCreatesTheFileWithTheGivenMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-key.pem")
	if err := Write(path, []byte("key material\n"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "key material\n" {
		t.Errorf("contents = %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want 0600", got)
		}
	}
}

func TestWriteReplacesAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.pem")
	if err := Write(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Write(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("Write (replace): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("contents = %q, want the replacement", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the target: %v", len(entries), entries)
	}
}

func TestWriteFailsOnAMissingDirectory(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "nope", "device.pem"), []byte("x"), 0o600); err == nil {
		t.Error("Write into a missing directory returned no error")
	}
}

// stubHost fakes the platform and the rename for the Windows-only branches,
// which the Linux and macOS runners cannot otherwise reach.
func stubHost(t *testing.T, goos string, rename func(string, string) error) {
	t.Helper()
	prevOS, prevRename := hostOS, renameFn
	hostOS, renameFn = goos, rename
	t.Cleanup(func() { hostOS, renameFn = prevOS, prevRename })
}

// errRefused stands in for a Windows rename refusal (ERROR_ACCESS_DENIED,
// ERROR_SHARING_VIOLATION, ERROR_FILE_NOT_FOUND) on runners that cannot
// produce one.
var errRefused = errors.New("Access is denied.")

// stubRenameRetry makes errRefused a retryable rename error and runs the retry
// loop on a fake clock, returning the sleeps it took. The Windows retry policy
// then runs, instantly, on any host.
func stubRenameRetry(t *testing.T) *[]time.Duration {
	t.Helper()
	prevRetryable, prevSleep, prevNow := isRetryableRenameErr, sleepFn, nowFn
	now := time.Unix(0, 0)
	var slept []time.Duration
	isRetryableRenameErr = func(err error) bool { return errors.Is(err, errRefused) }
	nowFn = func() time.Time { return now }
	sleepFn = func(d time.Duration) { slept = append(slept, d); now = now.Add(d) }
	t.Cleanup(func() { isRetryableRenameErr, sleepFn, nowFn = prevRetryable, prevSleep, prevNow })
	return &slept
}

func sum(ds []time.Duration) (total time.Duration) {
	for _, d := range ds {
		total += d
	}
	return total
}

func TestWriteRetriesARenameWindowsRefused(t *testing.T) {
	slept := stubRenameRetry(t)
	calls := 0
	stubHost(t, "windows", func(from, to string) error {
		calls++
		// Refused for well over the old 150 ms retry window.
		if calls < 10 {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
		}
		return os.Rename(from, to)
	})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write gave up on a transient Windows rename refusal: %v", err)
	}
	if calls != 10 {
		t.Errorf("rename called %d times, want 10 (nine refusals, then success)", calls)
	}
	if total := sum(*slept); total <= 150*time.Millisecond || total > renameRetryBudget {
		t.Errorf("slept %v in total, want more than the old 150 ms and within %v", total, renameRetryBudget)
	}
	for i := 1; i < len(*slept); i++ {
		if prev, cur := (*slept)[i-1], (*slept)[i]; cur < prev || cur > renameRetryMaxSleep {
			t.Errorf("sleep %d = %v after %v: want growing sleeps capped at %v", i, cur, prev, renameRetryMaxSleep)
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Errorf("contents = %q (err %v), want %q", data, err, "new")
	}
}

func TestWriteGivesUpOnAWindowsRenameAfterTheBudget(t *testing.T) {
	slept := stubRenameRetry(t)
	calls := 0
	var last error
	stubHost(t, "windows", func(from, to string) error {
		calls++
		last = &os.LinkError{Op: "rename", Old: from, New: fmt.Sprint(to, "#", calls), Err: errRefused}
		return last
	})
	dir := t.TempDir()
	err := Write(filepath.Join(dir, "config.json"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("Write succeeded although every rename was refused")
	}
	if err != last {
		t.Errorf("Write returned %v, want the last rename error %v", err, last)
	}
	if total := sum(*slept); total != renameRetryBudget {
		t.Errorf("retried for %v, want the whole %v budget", total, renameRetryBudget)
	}
	if calls != len(*slept)+1 {
		t.Errorf("rename called %d times for %d sleeps, want one more than the sleeps", calls, len(*slept))
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("ReadDir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("a failed write left %v behind", entries)
	}
}

func TestWriteDoesNotRetryANonTransientWindowsRename(t *testing.T) {
	slept := stubRenameRetry(t)
	calls := 0
	permanent := errors.New("The directory is not empty.")
	stubHost(t, "windows", func(from, to string) error {
		calls++
		if calls == 1 {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
		}
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: permanent}
	})
	err := Write(filepath.Join(t.TempDir(), "config.json"), []byte("x"), 0o600)
	if !errors.Is(err, permanent) {
		t.Fatalf("Write returned %v, want the non-retryable rename error", err)
	}
	if calls != 2 || len(*slept) != 1 {
		t.Errorf("rename called %d times after %d sleeps, want 2 after 1: a non-retryable error ends the retry", calls, len(*slept))
	}
}

// stubOwner fakes the geteuid/chown seams WritePreservingOwner uses to keep a
// replaced file's owner when running as root, since the test suite cannot
// actually run as root.
func stubOwner(t *testing.T, euid int, chownFn func(string, int, int) error) {
	t.Helper()
	prevEuid, prevChown := geteuid, chown
	geteuid = func() int { return euid }
	chown = chownFn
	t.Cleanup(func() { geteuid, chown = prevEuid, prevChown })
}

func TestWritePreservingOwnerChownsTheTempFileToTheExistingOwner(t *testing.T) {
	var gotPath string
	var gotUID, gotGID int
	called := false
	stubOwner(t, 0, func(name string, uid, gid int) error {
		called = true
		gotPath, gotUID, gotGID = name, uid, gid
		return nil
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantUID, wantGID := os.Getuid(), os.Getgid() // the file's real owner

	if err := WritePreservingOwner(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WritePreservingOwner: %v", err)
	}
	if !called {
		t.Fatal("WritePreservingOwner never chowned although geteuid was stubbed to 0")
	}
	if gotPath == path || gotPath == "" || filepath.Dir(gotPath) != dir {
		t.Errorf("chown target = %q, want the temp file in %q, not the target %q", gotPath, dir, path)
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("chown(uid=%d,gid=%d), want the existing file's owner (%d,%d)", gotUID, gotGID, wantUID, wantGID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("contents = %q, want new", data)
	}
}

func TestWritePreservingOwnerChownsToTheDirOwnerForANewFile(t *testing.T) {
	var gotUID, gotGID int
	called := false
	stubOwner(t, 0, func(_ string, uid, gid int) error {
		called = true
		gotUID, gotGID = uid, gid
		return nil
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")    // does not exist yet
	wantUID, wantGID := os.Getuid(), os.Getgid() // the directory's real owner

	if err := WritePreservingOwner(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WritePreservingOwner: %v", err)
	}
	if !called {
		t.Fatal("WritePreservingOwner never chowned a new file's temp file")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("chown(uid=%d,gid=%d), want the directory's owner (%d,%d)", gotUID, gotGID, wantUID, wantGID)
	}
}

func TestWritePreservingOwnerSkipsChownWhenNotRoot(t *testing.T) {
	called := false
	stubOwner(t, 501, func(string, int, int) error { called = true; return nil })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := WritePreservingOwner(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WritePreservingOwner: %v", err)
	}
	if called {
		t.Error("WritePreservingOwner chowned although geteuid was not 0")
	}
}

func TestWriteNeverChownsEvenAsRoot(t *testing.T) {
	called := false
	stubOwner(t, 0, func(string, int, int) error { called = true; return nil })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if called {
		t.Error("Write chowned the file; it must stay owner-agnostic for the device agent's key/cert writes")
	}
}

func TestWriteDoesNotRetryARenameElsewhere(t *testing.T) {
	calls := 0
	stubHost(t, "linux", func(string, string) error {
		calls++
		return errors.New("rename refused")
	})
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "config.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("Write succeeded although the rename failed")
	}
	if calls != 1 {
		t.Errorf("rename called %d times off Windows, want exactly 1", calls)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed write left %v behind", entries)
	}
}
