package flock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAcquireTimesOutWhileAnotherHolderHasTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	start := time.Now()
	if _, err := Acquire(path, 150*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("contended Acquire = %v, want ErrTimeout", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("contended Acquire gave up after %s, before its 150ms timeout", waited)
	}
	release()
	again, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	again()
}

func TestAcquireWaitsForARelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	next, err := Acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire did not pick up a lock released while it waited: %v", err)
	}
	next()
}

// stubOwner fakes the geteuid/chown seams Acquire uses to keep a freshly
// created lock file owned by the config directory's owner when running as
// root, since the test suite cannot actually run as root.
func stubOwner(t *testing.T, euid int, chownFn func(string, int, int) error) {
	t.Helper()
	prevEuid, prevChown := geteuid, chown
	geteuid = func() int { return euid }
	chown = chownFn
	t.Cleanup(func() { geteuid, chown = prevEuid, prevChown })
}

func TestAcquireChownsTheLockFileToTheDirOwnerWhenRoot(t *testing.T) {
	var gotPath string
	var gotUID, gotGID int
	called := false
	stubOwner(t, 0, func(name string, uid, gid int) error {
		called = true
		gotPath, gotUID, gotGID = name, uid, gid
		return nil
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.lock")
	wantUID, wantGID := os.Getuid(), os.Getgid() // the temp dir's real owner

	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if !called {
		t.Fatal("Acquire never chowned the lock file although geteuid was stubbed to 0")
	}
	if gotPath != path {
		t.Errorf("chown target = %q, want the lock file %q", gotPath, path)
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("chown(uid=%d,gid=%d), want the directory's owner (%d,%d)", gotUID, gotGID, wantUID, wantGID)
	}
}

func TestAcquireSkipsChownWhenNotRoot(t *testing.T) {
	called := false
	stubOwner(t, 501, func(string, int, int) error { called = true; return nil })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.lock")
	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if called {
		t.Error("Acquire chowned the lock file although geteuid was not 0")
	}
}

func TestAcquireCreatesAPrivateLockFileAndReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat lock file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("lock file mode = %v, want 0600", got)
		}
	}
	release()
	release() // a second release must be harmless
}
