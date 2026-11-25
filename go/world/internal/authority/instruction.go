package authority

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"
)

// SignedInstruction is an immutable operator decision. Its lifetime is separate
// from the short-lived, nonce-protected HTTP request which delivers it.
type SignedInstruction struct {
	Version         int             `json:"version"`
	Grant           SignedGrant     `json:"grant"`
	Audience        string          `json:"audience"`
	Operation       string          `json:"operation"`
	Payload         json.RawMessage `json:"payload"`
	IssuedAtUnixMS  int64           `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64           `json:"expires_at_unix_ms"`
	Signature       []byte          `json:"signature"`
}

func instructionPayload(i SignedInstruction) any {
	g, _ := json.Marshal(i.Grant)
	return struct {
		Version     int             `json:"version"`
		GrantDigest string          `json:"grant_digest"`
		Audience    string          `json:"audience"`
		Operation   string          `json:"operation"`
		Payload     json.RawMessage `json:"payload"`
		Issued      int64           `json:"issued"`
		Expires     int64           `json:"expires"`
	}{i.Version, hash(g), i.Audience, i.Operation, i.Payload, i.IssuedAtUnixMS, i.ExpiresAtUnixMS}
}

func SignInstruction(g SignedGrant, key ed25519.PrivateKey, audience, operation string, payload json.RawMessage, issuedAt time.Time, lifetime time.Duration) (SignedInstruction, error) {
	if !privateMatches(key, g.Grant.PublicKey) || g.Grant.Kind != "operator" || !contains(g.Grant.Scopes, operation) || (operation != "mission" && operation != "stop") || audience == "" || len(audience) > 256 || len(payload) == 0 || len(payload) > MaxEnvelopeBytes/2 || !json.Valid(payload) || lifetime < time.Millisecond || lifetime > DefaultRecordLifetime {
		return SignedInstruction{}, ErrDenied
	}
	i := SignedInstruction{Version: Version, Grant: g, Audience: audience, Operation: operation, Payload: append(json.RawMessage(nil), payload...), IssuedAtUnixMS: issuedAt.UnixMilli(), ExpiresAtUnixMS: issuedAt.Add(lifetime).UnixMilli()}
	var err error
	i.Signature, err = sign(key, "instruction", instructionPayload(i))
	return i, err
}

// VerifyInstruction checks current operator authority and revocation on every
// call, including ongoing execution. HTTP nonce replay protection is separate;
// task IDs and durable terminal state make delivery retries idempotent.
func (v *Verifier) VerifyInstruction(i SignedInstruction, operation string) (Identity, error) {
	return v.VerifyInstructionContext(context.Background(), i, operation)
}
func (v *Verifier) VerifyInstructionContext(ctx context.Context, i SignedInstruction, operation string) (Identity, error) {
	return v.VerifyInstructionForAudienceContext(ctx, i, operation, v.cfg.Audience)
}

// VerifyInstructionForAudience checks current authority for an explicitly
// configured final assignee gateway. Relays must obtain audience from trusted
// route configuration, never use the instruction's audience as the expectation.
// The original destination remains signed and is not rewritten for each hop.
func (v *Verifier) VerifyInstructionForAudience(i SignedInstruction, operation, audience string) (Identity, error) {
	return v.VerifyInstructionForAudienceContext(context.Background(), i, operation, audience)
}
func (v *Verifier) VerifyInstructionForAudienceContext(ctx context.Context, i SignedInstruction, operation, audience string) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	return v.verifyInstructionContext(ctx, i, operation, audience, false)
}

// VerifyInstructionHistoryForAudience checks the original signature and grant
// at issuance for retained task history. It allows expired instructions and
// grants, and does not apply current operator revocations. It therefore MUST
// NOT authorize admission, dispatch, continued execution, or a new stop. The
// assignee's separately signed status attests to the recorded outcome. This
// check is not independent proof of historical revocation state or admission.
func (v *Verifier) VerifyInstructionHistoryForAudience(i SignedInstruction, operation, audience string) (Identity, error) {
	v.lock()
	defer v.mu.Unlock()
	return v.verifyInstruction(i, operation, audience, true)
}

func (v *Verifier) verifyInstruction(i SignedInstruction, operation, audience string, history bool) (Identity, error) {
	return v.verifyInstructionContext(context.Background(), i, operation, audience, history)
}
func (v *Verifier) verifyInstructionContext(ctx context.Context, i SignedInstruction, operation, audience string, history bool) (Identity, error) {
	now := v.cfg.Now()
	if audience == "" || len(audience) > 256 || i.Version != Version || i.Audience != audience || i.Operation != operation || (operation != "mission" && operation != "stop") || i.Grant.Grant.Kind != "operator" || len(i.Payload) == 0 || len(i.Payload) > MaxEnvelopeBytes/2 || !json.Valid(i.Payload) || !validWindow(i.IssuedAtUnixMS, i.ExpiresAtUnixMS, DefaultRecordLifetime) || now.UnixMilli() < i.IssuedAtUnixMS || (!history && now.UnixMilli() >= i.ExpiresAtUnixMS) || !verify(i.Grant.Grant.PublicKey, "instruction", instructionPayload(i), i.Signature) {
		return Identity{}, ErrDenied
	}
	if history {
		g := i.Grant.Grant
		if err := validateGrant(g, v.cfg.MaxGrantLifetime); err != nil {
			return Identity{}, err
		}
		if !verify(v.cfg.RootPublicKey, "grant", g, i.Grant.Signature) || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != v.cfg.Domain || !contains(g.Scopes, operation) || i.IssuedAtUnixMS < g.NotBeforeUnixMS || (i.IssuedAtUnixMS >= g.NotAfterUnixMS && operation != "stop") {
			return Identity{}, ErrDenied
		}
		id := grantIdentity(g)
		if operation == "stop" || time.UnixMilli(i.ExpiresAtUnixMS).Before(id.NotAfter) {
			id.NotAfter = time.UnixMilli(i.ExpiresAtUnixMS)
		}
		return id, nil
	}
	// Require authority both when signed and now. An expired role can still stop.
	if _, err := v.identityContext(ctx, i.Grant, operation, time.UnixMilli(i.IssuedAtUnixMS), now); err != nil {
		return Identity{}, err
	}
	_, checkpoint, err := v.validateIdentity(i.Grant, operation, now, now)
	if err != nil {
		return Identity{}, err
	}
	if err = replayCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return Identity{}, err
	}
	// Preserve the durable admission floor above. Check the committed policy
	// and floor before the final sample: even this read-only call can wait for
	// another replay-store operation. A concurrent floor advance may deny this
	// older sample; it must never grant authority without a durable checkpoint.
	if err = replayValidateCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return Identity{}, err
	}
	completed := v.cfg.Now()
	// Compare wall time explicitly. Go's monotonic metadata must not hide a
	// wall-clock regression, including one within the persisted millisecond.
	if completed.Round(0).Before(now.Round(0)) || completed.UnixMilli() < i.IssuedAtUnixMS || completed.UnixMilli() >= i.ExpiresAtUnixMS {
		return Identity{}, ErrDenied
	}
	// No replay I/O follows this sample. Under v.mu the signed revocation list
	// is unchanged, so this check can only narrow the authority already admitted
	// at now. The later completion time is not a new persisted clock floor.
	id, _, err := v.validateIdentity(i.Grant, operation, completed, completed)
	if err != nil {
		return Identity{}, err
	}
	if operation == "stop" || time.UnixMilli(i.ExpiresAtUnixMS).Before(id.NotAfter) {
		id.NotAfter = time.UnixMilli(i.ExpiresAtUnixMS)
	}
	return id, nil
}
