package authority

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"time"
)

func (v *Verifier) lockContext(ctx context.Context) *diagnostics.Span {
	span := diagnostics.Begin(ctx, diagnostics.VerifierLock)
	v.lock()
	span.End()
	return diagnostics.Begin(ctx, diagnostics.VerifierHold)
}

// Optional context methods preserve custom ReplayStore compatibility. Context
// carries diagnostic correlation only; it never cancels a durable operation.
type replayContext interface {
	CheckpointContext(context.Context, uint64, string, time.Time) error
	ConsumeContext(context.Context, string, time.Time, time.Time) error
	ValidateCheckpointContext(context.Context, uint64, string, time.Time) error
}

func replayCheckpoint(ctx context.Context, s ReplayStore, rev uint64, d string, now time.Time) error {
	if r, ok := s.(*ReplayCache); ok && diagnostics.Enabled(ctx) {
		return r.CheckpointContext(ctx, rev, d, now)
	}
	return s.Checkpoint(rev, d, now)
}
func replayConsume(ctx context.Context, s ReplayStore, k string, expiry, now time.Time) error {
	if r, ok := s.(*ReplayCache); ok && diagnostics.Enabled(ctx) {
		return r.ConsumeContext(ctx, k, expiry, now)
	}
	return s.Consume(k, expiry, now)
}
func replayValidateCheckpoint(ctx context.Context, s ReplayStore, rev uint64, d string, now time.Time) error {
	if r, ok := s.(*ReplayCache); ok && diagnostics.Enabled(ctx) {
		return r.ValidateCheckpointContext(ctx, rev, d, now)
	}
	return s.ValidateCheckpoint(rev, d, now)
}
func replayAdmission(ctx context.Context, s ReplayAdmissionStore, rev uint64, d, k string, expiry, now time.Time) error {
	if r, ok := s.(*ReplayCache); ok && diagnostics.Enabled(ctx) {
		return r.CheckpointAndConsumeContext(ctx, rev, d, k, expiry, now)
	}
	return s.CheckpointAndConsume(rev, d, k, expiry, now)
}
