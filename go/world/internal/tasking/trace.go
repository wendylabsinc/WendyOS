package tasking

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

func (s *Store) lockContext(ctx context.Context) *diagnostics.Span {
	wait := diagnostics.Begin(ctx, diagnostics.TaskLock)
	s.mu.Lock()
	wait.End()
	return diagnostics.Begin(ctx, diagnostics.TaskHold)
}
func (s *Store) validateAuthority(ctx context.Context, r Record) (err error) {
	phase := diagnostics.Begin(ctx, diagnostics.MissionCheck)
	defer func() { phase.End(err) }()
	if s.cfg.ValidateAuthorityContext != nil {
		return s.cfg.ValidateAuthorityContext(ctx, r)
	}
	return s.cfg.ValidateAuthority(r)
}
func (s *Store) validateSupervisor(ctx context.Context, p json.RawMessage) (a supervision.Ack, o supervision.Operator, err error) {
	phase := diagnostics.Begin(ctx, diagnostics.SupervisorCheck)
	defer func() { phase.End(err) }()
	if s.cfg.ValidateSupervisorContext != nil {
		return s.cfg.ValidateSupervisorContext(ctx, p)
	}
	return s.cfg.ValidateSupervisor(p)
}

func (s *Store) validateSupervisors(ctx context.Context, proofs []json.RawMessage) (results []supervision.Verification, err error) {
	phase := diagnostics.Begin(ctx, diagnostics.SupervisorCheck)
	defer func() { phase.End(err) }()
	return s.cfg.ValidateSupervisorsContext(ctx, proofs)
}
