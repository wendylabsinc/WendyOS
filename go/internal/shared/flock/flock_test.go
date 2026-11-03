//go:build darwin || linux || windows

package flock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockContendsAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if locked, err := TryLock(a); err != nil || !locked {
		t.Fatalf("first TryLock = (%v, %v), want acquired", locked, err)
	}
	if locked, err := TryLock(b); err != nil || locked {
		t.Fatalf("contended TryLock = (%v, %v), want (false, nil)", locked, err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if locked, err := TryLock(b); err != nil || !locked {
		t.Fatalf("TryLock after release = (%v, %v), want acquired", locked, err)
	}
}

func TestLockWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocking.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := Lock(a); err != nil {
		t.Fatal(err)
	}
	defer Unlock(a)
	done := make(chan error, 1)
	go func() { done <- Lock(b) }()
	select {
	case err := <-done:
		t.Fatalf("Lock returned before release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if err := Unlock(b); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not acquire after release")
	}
}
