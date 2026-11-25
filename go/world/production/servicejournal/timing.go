package servicejournal

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	bolt "go.etcd.io/bbolt"
)

func (j *Journal) lock(ctx context.Context) {
	done := rpctrace.Start(ctx, rpctrace.StageJournalWait)
	j.mu.Lock()
	done()
}

func (j *Journal) commitTrace(ctx context.Context, fn func(*bolt.Tx) error) error {
	defer rpctrace.Start(ctx, rpctrace.StageJournalCommit)()
	return j.commit(fn)
}

// Replica.Now has no context. These operations may include its source-clock
// HTTP work; their inclusive durations do not claim individual HTTP counts.
func traceValue[T any](ctx context.Context, stage rpctrace.Stage, fn func() (T, error)) (T, error) {
	defer rpctrace.Start(ctx, stage)()
	return fn()
}
