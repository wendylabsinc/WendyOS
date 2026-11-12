package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// releaseResetter removes every registry release (services.Postbox, which
// also stops its pulls first). imagesOnly leaves the containers in place.
type releaseResetter interface {
	RemoveAllReleases(ctx context.Context, imagesOnly bool) error
}

// resetRuntimeTries is how often the reset recovery tries without a container
// runtime before it gives up on this process: the runtime client is made once
// at start, so only a restart can reconnect it.
const resetRuntimeTries = 3

// containerdRoot holds containerd's content and snapshots by default.
const containerdRoot = "/var/lib/containerd"

// resetReleaseCleanup is the reset step that deletes every registry release,
// decrypted layers included, before a reset is confirmed: through the postbox
// when it runs, else through the container control (removeAll). imagesOnly
// deletes only the release images (the net shutdown has not drained).
//
// Without a container runtime on Linux the releases may still be on disk, so
// the reset stays unconfirmed, and after resetRuntimeTries failures giveUp
// is called (nil: never) so the service manager restarts the agent, which
// connects again. A host with neither the runtime socket nor its state
// directory never held a release: nothing to remove.
func resetReleaseCleanup(goos string, runtimeErr error, socket string, postbox releaseResetter, removeAll func(ctx context.Context, imagesOnly bool) error, giveUp func(error)) func(ctx context.Context, imagesOnly bool) error {
	failures := 0
	return func(ctx context.Context, imagesOnly bool) error {
		switch {
		case postbox != nil:
			return postbox.RemoveAllReleases(ctx, imagesOnly)
		case runtimeErr == nil:
			return removeAll(ctx, imagesOnly)
		case goos != "linux", !exists(socket) && !exists(containerdRoot):
			return nil
		}
		err := fmt.Errorf("container runtime unavailable, registry releases not removed: %w", runtimeErr)
		if failures++; failures >= resetRuntimeTries && giveUp != nil {
			giveUp(err)
		}
		return err
	}
}

// exists reports false only when path is certainly absent.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
