package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeResetter struct {
	calls      int
	imagesOnly bool
}

func (f *fakeResetter) RemoveAllReleases(_ context.Context, imagesOnly bool) error {
	f.calls++
	f.imagesOnly = imagesOnly
	return nil
}

func TestResetReleaseCleanup(t *testing.T) {
	removed, imagesOnly := 0, false
	removeAll := func(_ context.Context, only bool) error { removed++; imagesOnly = only; return nil }
	down := errors.New("containerd down")
	socket := filepath.Join(t.TempDir(), "containerd.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	pb := &fakeResetter{}
	if err := resetReleaseCleanup("linux", nil, socket, pb, removeAll, nil)(context.Background(), true); err != nil || pb.calls != 1 || !pb.imagesOnly || removed != 0 {
		t.Fatalf("with a postbox: err %v, postbox calls %d (images only %v), direct %d", err, pb.calls, pb.imagesOnly, removed)
	}
	if err := resetReleaseCleanup("linux", nil, socket, nil, removeAll, nil)(context.Background(), false); err != nil || removed != 1 || imagesOnly {
		t.Fatalf("at restart recovery: err %v, direct %d (images only %v)", err, removed, imagesOnly)
	}
	if err := resetReleaseCleanup("linux", down, socket, nil, removeAll, nil)(context.Background(), false); err == nil || removed != 1 {
		t.Fatalf("no runtime on Linux: err %v, direct %d: want the reset held", err, removed)
	}
	if err := resetReleaseCleanup("darwin", down, socket, nil, removeAll, nil)(context.Background(), false); err != nil {
		t.Fatalf("no runtime off Linux: %v", err)
	}
}

// N1: the runtime client is made once at start, so the recovery gives up
// after a few tries and the service manager restarts the agent.
func TestResetReleaseCleanupGivesUpWithoutARuntime(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "containerd.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gaveUp := 0
	cleanup := resetReleaseCleanup("linux", errors.New("containerd down"), socket, nil, nil, func(error) { gaveUp++ })
	for i := 1; i <= resetRuntimeTries; i++ {
		if err := cleanup(context.Background(), false); err == nil {
			t.Fatalf("try %d: want the reset held", i)
		}
		if want := max(0, i-resetRuntimeTries+1); gaveUp != want {
			t.Fatalf("try %d: gave up %d times, want %d", i, gaveUp, want)
		}
	}
}

// N1: a host with no containerd socket and no containerd state never held
// a release.
func TestResetReleaseCleanupWithoutContainerdHasNothingToRemove(t *testing.T) {
	if exists(containerdRoot) {
		t.Skip("this host has containerd state")
	}
	cleanup := resetReleaseCleanup("linux", errors.New("no socket"), filepath.Join(t.TempDir(), "missing.sock"), nil, nil, func(error) { t.Fatal("gave up") })
	if err := cleanup(context.Background(), false); err != nil {
		t.Fatalf("no containerd at all: %v", err)
	}
}
