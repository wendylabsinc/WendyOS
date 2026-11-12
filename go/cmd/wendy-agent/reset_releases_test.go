package main

import (
	"context"
	"errors"
	"testing"
)

type fakeResetter struct{ calls int }

func (f *fakeResetter) RemoveAllReleases(context.Context) error { f.calls++; return nil }

func TestResetReleaseCleanup(t *testing.T) {
	removed := 0
	removeAll := func(context.Context) error { removed++; return nil }
	down := errors.New("containerd down")

	pb := &fakeResetter{}
	if err := resetReleaseCleanup("linux", nil, pb, removeAll)(context.Background()); err != nil || pb.calls != 1 || removed != 0 {
		t.Fatalf("with a postbox: err %v, postbox calls %d, direct %d", err, pb.calls, removed)
	}
	if err := resetReleaseCleanup("linux", nil, nil, removeAll)(context.Background()); err != nil || removed != 1 {
		t.Fatalf("at restart recovery: err %v, direct %d", err, removed)
	}
	if err := resetReleaseCleanup("linux", down, nil, removeAll)(context.Background()); err == nil || removed != 1 {
		t.Fatalf("no runtime on Linux: err %v, direct %d: want the reset held", err, removed)
	}
	if err := resetReleaseCleanup("darwin", down, nil, removeAll)(context.Background()); err != nil {
		t.Fatalf("no runtime off Linux: %v", err)
	}
}
