package proofarchive

import (
	"context"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	bolt "go.etcd.io/bbolt"
)

// These spans only measure existing work. In particular archive_commit includes
// the complete synchronous transaction callback, so its nested verification and
// usage durations are not disjoint costs. No writer runs on this call path.
func (a *Archive) commitTrace(ctx context.Context, fn func(*bolt.Tx) error) error {
	defer rpctrace.Start(ctx, rpctrace.StageArchiveCommit)()
	return a.commit(fn)
}

func (a *Archive) usageTrace(ctx context.Context, tx *bolt.Tx) (usage, error) {
	defer rpctrace.Start(ctx, rpctrace.StageArchiveUsage)()
	return a.usage(tx)
}
