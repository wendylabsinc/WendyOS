package livefusion

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

// Reserve before the mutation, including a response that may never arrive.
// Both maps live for this bounded Session; neither changes source high-water
// marks, authority bindings, original lifetimes or the Agent's source counter.
func (s *Session) reserveNativePublication(proofs []inputProof, inputDigest string) (bool, error) {
	if !s.native || s.window == nil || !sensing.DigestValid(inputDigest) {
		return false, ErrSource
	}
	if err := s.window.retire(proofs); err != nil {
		return false, err
	}
	if _, attempted := s.attempted[inputDigest]; attempted {
		return false, nil
	}
	if len(s.attempted) >= s.cfg.MaxTicks {
		return false, ErrSource
	}
	s.attempted[inputDigest] = struct{}{}
	return true, nil
}

// This is continuation of sensing, not a retry of the mutation. A refused or
// malformed acknowledgment, scope error or canceled session remains terminal.
func (s *Session) continueUnknown(ctx context.Context, out Result, err error) bool {
	if !s.native || ctx == nil || ctx.Err() != nil || !errors.Is(err, ErrUncertain) || out.FailureStage != "publish_result" || out.Diagnostics == nil {
		return false
	}
	p := out.Diagnostics.Publication
	return p != nil && p.Attempts == 1 && p.FailureStage == "rpc" &&
		(p.GRPCCode == "Unavailable" || p.GRPCCode == "DeadlineExceeded")
}
