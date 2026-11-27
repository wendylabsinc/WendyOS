package tasking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

var ErrSupervisionVerification = errors.New("invalid supervisor batch verification result")

type supervisorInput struct {
	key    supervisionKey
	active activeSupervision
	digest string
}

// Called under s.mu. Losing the volatile coverage precedes any fallible clock
// or persistence work. A failed latch therefore cannot bridge this gap through
// a later challenge/ACK, even if the verifier's policy subsequently recovers.
func (s *Store) failSupervisorBatch(ctx context.Context, taskID string, cause error) error {
	for key := range s.supervision {
		if key.task == taskID {
			delete(s.supervision, key)
		}
	}
	wall, mono, err := s.clock()
	if err != nil {
		return errors.Join(cause, err)
	}
	return errors.Join(cause, s.enforceSupervisionContinuityAtContext(ctx, taskID, wall, mono))
}

// Each chunk is a fresh authority transaction, not an atomic policy snapshot
// across all operators. No result survives this stage or crosses task commit.
func (s *Store) supervisionBatchDeadlineContext(ctx context.Context, taskID string) (time.Duration, error) {
	fail := func(err error) (time.Duration, error) { return 0, s.failSupervisorBatch(ctx, taskID, err) }
	wall, mono, err := s.clock()
	if err != nil {
		return fail(err)
	}
	s.pruneSupervision(wall, mono)
	inputs := make([]supervisorInput, 0, s.cfg.MaxSupervisors)
	for key, slot := range s.supervision {
		if key.task != taskID || slot.active == nil {
			continue
		}
		active := *slot.active
		if len(active.proof) == 0 || len(active.proof) > supervision.MaxVerificationProofBytes || !json.Valid(active.proof) {
			return fail(ErrSupervisionVerification)
		}
		active.proof = append(json.RawMessage(nil), active.proof...)
		digest := sha256.Sum256(active.proof)
		inputs = append(inputs, supervisorInput{key: key, active: active, digest: hex.EncodeToString(digest[:])})
	}
	if len(inputs) == 0 {
		return 0, nil
	}
	if len(inputs) > s.cfg.MaxSupervisors || len(inputs) > 32 {
		return fail(ErrSupervisionVerification)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].key.principal < inputs[j].key.principal })
	results := make([]supervision.Verification, 0, len(inputs))
	for start := 0; start < len(inputs); start += supervision.MaxVerificationProofs {
		end := min(start+supervision.MaxVerificationProofs, len(inputs))
		proofs := make([]json.RawMessage, end-start)
		bytesTotal := 0
		for i := start; i < end; i++ {
			proofs[i-start] = append(json.RawMessage(nil), inputs[i].active.proof...)
			bytesTotal += len(proofs[i-start])
		}
		if bytesTotal > supervision.MaxVerificationBytes {
			return fail(ErrSupervisionVerification)
		}
		verified, verifyErr := s.validateSupervisors(ctx, proofs)
		if verifyErr != nil {
			return fail(verifyErr)
		}
		if len(verified) != len(proofs) {
			return fail(ErrSupervisionVerification)
		}
		for i, result := range verified {
			input := inputs[start+i]
			if result.InputSHA256 != input.digest {
				return fail(ErrSupervisionVerification)
			}
			if result.Denied {
				if result.Ack != (supervision.Ack{}) || result.Operator.Principal != "" || result.Operator.KeySHA256 != "" || result.Operator.GrantID != "" || result.Operator.ValidUntilUnixMS != 0 || len(result.Operator.Proof) != 0 {
					return fail(ErrSupervisionVerification)
				}
			} else {
				c := result.Ack.Challenge
				r, ok := s.state.Records[taskID]
				if !ok || c != input.active.challenge || !operatorMatches(result.Operator, c) ||
					c.Version != supervision.Version || c.Tenant != s.cfg.Tenant || c.Site != s.cfg.Site || c.Domain != string(s.cfg.Domain) ||
					c.RunID != s.cfg.RunID || c.Audience != s.cfg.Audience || c.Assignee != s.cfg.Assignee || c.GatewayPrincipal != s.cfg.GatewayPrincipal ||
					c.TaskID != taskID || c.SpecSHA256 != hex.EncodeToString(r.Digest[:]) || c.GatewayBootID != s.supervisionBoot || c.ExecutorEpoch != s.state.ExecutorEpoch ||
					!nonempty(result.Operator.Principal, 512) || !sha256String(result.Operator.KeySHA256) || !nonempty(result.Operator.GrantID, 256) || result.Operator.ValidUntilUnixMS <= 0 ||
					!bytes.Equal(result.Operator.Proof, input.active.proof) {
					return fail(ErrSupervisionVerification)
				}
				result.Operator.Proof = append(json.RawMessage(nil), result.Operator.Proof...)
			}
			results = append(results, result)
		}
	}
	// All chunks and bindings must pass before applying selective denials. Time
	// spent in a later chunk may only shorten an earlier operator's fixed lease.
	wall, mono, err = s.clock()
	if err != nil {
		return fail(err)
	}
	for i, result := range results {
		input := inputs[i]
		slot := s.supervision[input.key]
		active := slot.active
		if result.Denied || !validOperator(result.Operator, wall) || mono >= active.deadline || wall >= active.expires {
			slot.active = nil
			continue
		}
		active.expires = min(active.expires, result.Operator.ValidUntilUnixMS)
		until, e := deadline(active.expires, wall, mono)
		if e != nil {
			slot.active = nil
			continue
		}
		active.deadline = min(active.deadline, until)
	}
	s.pruneSupervision(wall, mono)
	var until time.Duration
	for key, slot := range s.supervision {
		if key.task == taskID && slot.active != nil {
			until = max(until, slot.active.deadline)
		}
	}
	return until, nil
}
