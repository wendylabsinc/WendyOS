package livefusion

import (
	"context"
	"reflect"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// projectAdmitted is only reachable from an explicit native session after a
// complete current Query. Candidate history contains exact earlier broker
// reads, never caller-imported envelopes. The broker remains responsible for
// signatures and final current dependency admission; this projection performs
// no cryptographic verification and grants no renewed lifetime.
func (s *session) projectAdmitted(ctx context.Context, proofs []inputProof, signed []serviceorigin.SignedRecord) ([]serviceorigin.Record, error) {
	if ctx == nil || ctx.Err() != nil || !s.completeProofs(proofs) || len(signed) != len(proofs) {
		return nil, ErrSource
	}
	result := make([]serviceorigin.Record, len(proofs))
	for i, original := range proofs {
		if !reflect.DeepEqual(signed[i], original.signed) {
			return nil, ErrSource
		}
		p, err := s.parse(original.raw, s.sources)
		if err != nil || p.reference != original.reference || !reflect.DeepEqual(p.record, original.record) {
			return nil, ErrSource
		}
		current, exists := s.bindings[p.record.Publication.Stream]
		if !exists || !reflect.DeepEqual(current.scope, p.record.Scope) || current.epoch != p.record.Epoch ||
			current.delegation != p.record.DelegationSHA256 || p.record.Sequence > current.sequence ||
			p.record.Sequence == current.sequence && p.reference.EnvelopeSHA256 != current.envelope {
			return nil, ErrSource
		}
		var claims serviceorigin.DelegationClaims
		if jws.DecodeStrict(p.signed.Delegation.Claims, &claims) != nil || !reflect.DeepEqual(claims.Scope, p.record.Scope) ||
			claims.Epoch != p.record.Epoch || claims.IssuedAtUnixMS <= 0 || claims.ExpiresAtUnixMS <= claims.IssuedAtUnixMS ||
			p.record.IssuedAtUnixMS < claims.IssuedAtUnixMS || p.record.ExpiresAtUnixMS > claims.ExpiresAtUnixMS || p.record.ExpiresAtUnixMS <= p.record.IssuedAtUnixMS {
			return nil, ErrSource
		}
		result[i] = p.record
	}
	return result, ctx.Err()
}
