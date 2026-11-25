package serviceevidence

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func (s *Store) traceLock(ctx context.Context) {
	done := rpctrace.Start(ctx, rpctrace.StageEvidenceWait)
	s.mu.Lock()
	done()
}

func (s *Store) saveFloor(ctx context.Context, w trustedtime.Interval) error {
	defer rpctrace.Start(ctx, rpctrace.StageEvidenceFloor)()
	return s.saveState(w, false)
}
