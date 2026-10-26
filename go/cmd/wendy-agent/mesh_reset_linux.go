//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
)

// Called only with no live mesh workers. Reset recovery must remove old kernel
// policies even though an unenrolled agent never constructs a replacement node.
func cleanupResetMesh(ctx context.Context) error {
	sharingErr := meshsharing.CleanupStale(ctx)
	return errors.Join(sharingErr, localmesh.CleanupStaleKernel(), localmesh.CleanupStaleResolver(ctx))
}
