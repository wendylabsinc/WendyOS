//go:build !linux

package meshsharing

import "context"

// CleanupStale has no kernel state to withdraw outside Linux.
func CleanupStale(ctx context.Context) error { return ctx.Err() }
