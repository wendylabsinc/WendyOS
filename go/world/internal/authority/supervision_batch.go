package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

const (
	MaxSupervisionBatchProofs = supervision.MaxVerificationProofs
	MaxSupervisionBatchBytes  = supervision.MaxVerificationBytes
)

var (
	ErrSupervisionBatchBounds   = errors.New("supervision verification exceeds chunk bounds")
	ErrSupervisionClockRollback = errors.New("supervision verification clock regressed")
)

// VerifySupervisionAcksContext freshly verifies one bounded chunk against one
// configured gateway and one locked policy snapshot. Individual proof or gateway
// denials return ordered, empty Denied entries. Stale policy, clock or replay
// failures return nil results and an error when encountered. An initial gateway
// identity denial returns only denied entries without touching replay state; it
// need not discover an independently poisoned or advanced replay floor.
// No nonce is consumed. Successful
// entries require a durable initial checkpoint and completion-time checks after
// all replay I/O. Completion can narrow permission but does not advance the floor.
// Results must never be reused across task persistence or another stage.
func (v *Verifier) VerifySupervisionAcksContext(ctx context.Context, server SignedGrant, proofs []json.RawMessage) ([]supervision.Verification, error) {
	if len(proofs) == 0 || len(proofs) > supervision.MaxVerificationProofs {
		return nil, ErrSupervisionBatchBounds
	}
	total := 0
	for _, raw := range proofs {
		if len(raw) > supervision.MaxVerificationProofBytes {
			return nil, ErrSupervisionBatchBounds
		}
		total += len(raw)
	}
	if total > supervision.MaxVerificationBytes || !supervisionServerBounded(server) {
		return nil, ErrSupervisionBatchBounds
	}
	// Copy before waiting for shared verifier/replay locks. Do not normalize raw
	// JSON before hashing: the callback caller must recognize its exact inputs.
	group := make([]json.RawMessage, len(proofs))
	results := make([]supervision.Verification, len(proofs))
	for i, raw := range proofs {
		group[i] = bytes.Clone(raw)
		results[i] = supervision.Verification{InputSHA256: hash(group[i]), Denied: true}
	}
	server.Signature = bytes.Clone(server.Signature)
	server.Grant.PublicKey = bytes.Clone(server.Grant.PublicKey)
	server.Grant.Scopes = append([]string(nil), server.Grant.Scopes...)
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if err := v.supervisionBatchPolicy(now); err != nil {
		return nil, err
	}
	_, checkpoint, err := v.validateIdentity(server, "supervision-server", now, now)
	if err != nil {
		if errors.Is(err, ErrStaleRevocations) {
			return nil, err
		}
		return results, nil
	}
	// A well-signed grant with the wrong gateway kind/role cannot certify leases.
	// The initial identity above still carries the legacy gateway-first floor.
	gatewayBound := server.Grant.Kind == "device" && contains(server.Grant.Scopes, "task-status") && server.Grant.TaskStatusAudience == v.cfg.Audience && server.Grant.TaskStatusAssignee != ""
	parsed := make([]SignedSupervisionAck, len(group))
	eligible := make([]bool, len(group))
	if gatewayBound {
		for i, raw := range group {
			s := &parsed[i]
			if DecodeStrict(raw, s) != nil || s.Version != Version || !v.supervisionScope(s.Ack.Challenge, now) || !supervisionGrant(server.Grant, s.Ack.Challenge, true) || !supervisionGrant(s.Grant.Grant, s.Ack.Challenge, false) || !verify(s.Grant.Grant.PublicKey, "supervision-ack", supervisionPayload(s.Version, s.Grant, s.Ack), s.Signature) {
				continue
			}
			if _, _, err = v.validateIdentity(s.Grant, "supervise", now, now); err != nil {
				if errors.Is(err, ErrStaleRevocations) {
					return nil, err
				}
				continue
			}
			eligible[i] = true
		}
	}
	if err = replayCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return nil, err
	}
	if err = replayValidateCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return nil, err
	}
	completed := v.cfg.Now()
	if completed.Round(0).Before(now.Round(0)) {
		return nil, ErrSupervisionClockRollback
	}
	if err = v.supervisionBatchPolicy(completed); err != nil {
		return nil, err
	}
	if _, _, err = v.validateIdentity(server, "supervision-server", completed, completed); err != nil {
		if errors.Is(err, ErrStaleRevocations) {
			return nil, err
		}
		return results, nil
	}
	for i, s := range parsed {
		if !eligible[i] || !v.supervisionScope(s.Ack.Challenge, completed) {
			continue
		}
		id, _, err := v.validateIdentity(s.Grant, "supervise", completed, completed)
		if err != nil {
			if errors.Is(err, ErrStaleRevocations) {
				return nil, err
			}
			continue
		}
		canonical, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		results[i] = supervision.Verification{InputSHA256: results[i].InputSHA256, Ack: s.Ack, Operator: supervision.Operator{Principal: id.Principal, KeySHA256: id.SigningKeySHA256, GrantID: id.GrantID, ValidUntilUnixMS: id.NotAfter.UnixMilli(), Proof: canonical}}
	}
	return results, nil
}

// Check policy health before selective grant denials so an expired gateway does
// not hide a stale revocation list. The installed signature was checked on update.
func (v *Verifier) supervisionBatchPolicy(now time.Time) error {
	if v.rev == nil {
		return ErrStaleRevocations
	}
	r := v.rev.Revocations
	if now.UnixMilli() < r.IssuedAtUnixMS || now.UnixMilli() >= r.ExpiresAtUnixMS || now.Sub(time.UnixMilli(r.IssuedAtUnixMS)) > v.cfg.MaxRevocationAge {
		return ErrStaleRevocations
	}
	return nil
}

func supervisionServerBounded(server SignedGrant) bool {
	g := server.Grant
	total := len(server.Signature) + len(g.PublicKey)
	if total > MaxEnvelopeBytes || len(g.Scopes) > 16 {
		return false
	}
	for _, s := range []string{g.TaskStatusAssignee, g.TaskStatusAudience, g.ExecuteAssignee, g.PublishAudience, g.ID, g.Tenant, g.Site, g.Domain, g.Principal, g.Kind, g.Source.Device, g.Source.App, g.Source.Service, g.Source.Stream} {
		total += len(s)
		if total > MaxEnvelopeBytes {
			return false
		}
	}
	for _, s := range g.Scopes {
		total += len(s)
		if total > MaxEnvelopeBytes {
			return false
		}
	}
	return boundedSupervision(server)
}
