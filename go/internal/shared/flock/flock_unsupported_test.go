//go:build !darwin && !linux && !windows

package flock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnsupportedLocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if locked, err := TryLock(f); locked || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("TryLock = (%v, %v), want (false, ErrUnsupported)", locked, err)
	}
	if err := Unlock(f); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Unlock = %v, want ErrUnsupported", err)
	}
	if release, err := Acquire(path, time.Second); release != nil || !errors.Is(err, errors.ErrUnsupported) {
		if release != nil {
			release()
		}
		t.Fatalf("Acquire returned release=%t, error=%v; want no release and ErrUnsupported", release != nil, err)
	}
}
