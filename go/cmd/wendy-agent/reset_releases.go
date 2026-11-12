package main

import (
	"context"
	"fmt"
)

// releaseResetter removes every registry release (services.Postbox, which
// also stops its pulls first).
type releaseResetter interface {
	RemoveAllReleases(ctx context.Context) error
}

// resetReleaseCleanup is the reset step that deletes every registry release,
// decrypted layers included, before a reset is confirmed: through the postbox
// when it runs, else through the container control (removeAll). Without a
// container runtime on Linux the releases may still be on disk, so the reset
// stays unconfirmed and a restart retries it.
func resetReleaseCleanup(goos string, runtimeErr error, postbox releaseResetter, removeAll func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		switch {
		case postbox != nil:
			return postbox.RemoveAllReleases(ctx)
		case runtimeErr == nil:
			return removeAll(ctx)
		case goos == "linux":
			return fmt.Errorf("container runtime unavailable, registry releases not removed: %w", runtimeErr)
		}
		return nil
	}
}
