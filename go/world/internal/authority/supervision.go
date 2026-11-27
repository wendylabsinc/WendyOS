package authority

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

type SignedSupervisionChallenge struct {
	Version   int                   `json:"version"`
	Grant     SignedGrant           `json:"grant"`
	Challenge supervision.Challenge `json:"challenge"`
	Signature []byte                `json:"signature"`
}
type SignedSupervisionAck struct {
	Version   int             `json:"version"`
	Grant     SignedGrant     `json:"grant"`
	Ack       supervision.Ack `json:"ack"`
	Signature []byte          `json:"signature"`
}
type SignedSupervisionReceipt struct {
	Version   int                 `json:"version"`
	Grant     SignedGrant         `json:"grant"`
	Receipt   supervision.Receipt `json:"receipt"`
	Signature []byte              `json:"signature"`
}

func supervisionPayload(version int, grant SignedGrant, message any) any {
	b, _ := json.Marshal(grant)
	return struct {
		Version     int    `json:"version"`
		GrantSHA256 string `json:"grant_sha256"`
		Message     any    `json:"message"`
	}{version, hash(b), message}
}

func supervisionDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func supervisionToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func validSupervisionChallenge(c supervision.Challenge) bool {
	if c.Version != supervision.Version || c.Domain != "simulation" || c.ExecutorEpoch == 0 || !supervisionDigest(c.SpecSHA256) || !supervisionDigest(c.OperatorKeySHA256) || !validNonce(c.RequestID) || !validNonce(c.ChallengeID) || !validWindow(c.IssuedAtUnixMS, c.ExpiresAtUnixMS, supervision.MaxLease) {
		return false
	}
	for _, s := range []string{c.Tenant, c.Site, c.RunID, c.Audience, c.Assignee, c.GatewayPrincipal, c.GatewayBootID, c.TaskID, c.OperatorPrincipal, c.OperatorGrantID} {
		if !supervisionToken(s) {
			return false
		}
	}
	return true
}
func supervisionGrant(g Grant, c supervision.Challenge, server bool) bool {
	if validateGrant(g, 365*24*time.Hour) != nil || !validSupervisionChallenge(c) || g.Tenant != c.Tenant || g.Site != c.Site || g.Domain != c.Domain || c.IssuedAtUnixMS < g.NotBeforeUnixMS || c.ExpiresAtUnixMS > g.NotAfterUnixMS {
		return false
	}
	if server {
		return g.Kind == "device" && contains(g.Scopes, "supervision-server") && contains(g.Scopes, "task-status") && g.Principal == c.GatewayPrincipal && g.TaskStatusAudience == c.Audience && g.TaskStatusAssignee == c.Assignee
	}
	return g.Kind == "operator" && contains(g.Scopes, "supervise") && contains(g.Scopes, "stop") && g.Principal == c.OperatorPrincipal && KeyID(g.PublicKey) == c.OperatorKeySHA256 && g.ID == c.OperatorGrantID
}
func boundedSupervision(s any) bool {
	b, err := json.Marshal(s)
	return err == nil && len(b) <= MaxEnvelopeBytes
}

func SignSupervisionChallenge(g SignedGrant, key ed25519.PrivateKey, c supervision.Challenge) (SignedSupervisionChallenge, error) {
	s := SignedSupervisionChallenge{Version: Version, Grant: g, Challenge: c}
	if !privateMatches(key, g.Grant.PublicKey) || !supervisionGrant(g.Grant, c, true) {
		return SignedSupervisionChallenge{}, ErrDenied
	}
	var err error
	s.Signature, err = sign(key, "supervision-challenge", supervisionPayload(s.Version, g, c))
	if err != nil || !boundedSupervision(s) {
		return SignedSupervisionChallenge{}, ErrDenied
	}
	return s, nil
}
func SignSupervisionAck(g SignedGrant, key ed25519.PrivateKey, a supervision.Ack) (SignedSupervisionAck, error) {
	s := SignedSupervisionAck{Version: Version, Grant: g, Ack: a}
	if !privateMatches(key, g.Grant.PublicKey) || !supervisionGrant(g.Grant, a.Challenge, false) {
		return SignedSupervisionAck{}, ErrDenied
	}
	var err error
	s.Signature, err = sign(key, "supervision-ack", supervisionPayload(s.Version, g, a))
	if err != nil || !boundedSupervision(s) {
		return SignedSupervisionAck{}, ErrDenied
	}
	return s, nil
}
func SignSupervisionReceipt(g SignedGrant, key ed25519.PrivateKey, r supervision.Receipt) (SignedSupervisionReceipt, error) {
	s := SignedSupervisionReceipt{Version: Version, Grant: g, Receipt: r}
	if !privateMatches(key, g.Grant.PublicKey) || !supervisionGrant(g.Grant, r.Challenge, true) || !supervisionDigest(r.AckSHA256) {
		return SignedSupervisionReceipt{}, ErrDenied
	}
	var err error
	s.Signature, err = sign(key, "supervision-receipt", supervisionPayload(s.Version, g, r))
	if err != nil || !boundedSupervision(s) {
		return SignedSupervisionReceipt{}, ErrDenied
	}
	return s, nil
}

func (v *Verifier) supervisionScope(c supervision.Challenge, now time.Time) bool {
	return validSupervisionChallenge(c) && c.Tenant == v.cfg.Tenant && c.Site == v.cfg.Site && c.Domain == v.cfg.Domain && c.Audience == v.cfg.Audience && validWindow(c.IssuedAtUnixMS, c.ExpiresAtUnixMS, v.cfg.MaxRequestLifetime) && c.IssuedAtUnixMS <= now.UnixMilli() && now.UnixMilli() < c.ExpiresAtUnixMS
}

// VerifySupervisionChallenge authenticates a fresh gateway challenge. The caller
// must additionally match all requested task, run, boot, epoch and operator
// bindings, including its fresh RequestID. This method consumes no nonce.
func (v *Verifier) VerifySupervisionChallenge(s SignedSupervisionChallenge, expectedGateway string) (Identity, error) {
	return v.VerifySupervisionChallengeContext(context.Background(), s, expectedGateway)
}
func (v *Verifier) VerifySupervisionChallengeContext(ctx context.Context, s SignedSupervisionChallenge, expectedGateway string) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if s.Version != Version || expectedGateway == "" || s.Challenge.GatewayPrincipal != expectedGateway || !boundedSupervision(s) || !v.supervisionScope(s.Challenge, now) || !supervisionGrant(s.Grant.Grant, s.Challenge, true) || !verify(s.Grant.Grant.PublicKey, "supervision-challenge", supervisionPayload(s.Version, s.Grant, s.Challenge), s.Signature) {
		return Identity{}, ErrDenied
	}
	return v.identityContext(ctx, s.Grant, "supervision-server", now, now)
}

// VerifySupervisionAckProof returns a currently authorized ACK without consuming
// a request nonce. Grant checks may still persist the authority clock checkpoint. Tasking must match it against an issued, unexpired challenge and
// the current boot and executor epoch. It is deliberately safe to repeat during
// every evaluation: current grant expiry and revocation apply each time.
func (v *Verifier) VerifySupervisionAckProof(raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
	return v.VerifySupervisionAckProofContext(context.Background(), raw)
}
func (v *Verifier) VerifySupervisionAckProofContext(ctx context.Context, raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
	var s SignedSupervisionAck
	if err := DecodeStrict(raw, &s); err != nil {
		return supervision.Ack{}, supervision.Operator{}, err
	}
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if s.Version != Version || !v.supervisionScope(s.Ack.Challenge, now) || !supervisionGrant(s.Grant.Grant, s.Ack.Challenge, false) || !verify(s.Grant.Grant.PublicKey, "supervision-ack", supervisionPayload(s.Version, s.Grant, s.Ack), s.Signature) {
		return supervision.Ack{}, supervision.Operator{}, ErrDenied
	}
	id, err := v.identityContext(ctx, s.Grant, "supervise", now, now)
	if err != nil {
		return supervision.Ack{}, supervision.Operator{}, err
	}
	canonical, err := json.Marshal(s)
	if err != nil {
		return supervision.Ack{}, supervision.Operator{}, ErrDenied
	}
	return s.Ack, supervision.Operator{Principal: id.Principal, KeySHA256: id.SigningKeySHA256, GrantID: id.GrantID, ValidUntilUnixMS: id.NotAfter.UnixMilli(), Proof: canonical}, nil
}

// VerifySupervisionReceipt authenticates the gateway's fixed-deadline receipt.
// The caller must compare its entire Challenge and AckSHA256 to its own ACK;
// receiving this receipt must never extend the original challenge expiry.
func (v *Verifier) VerifySupervisionReceipt(s SignedSupervisionReceipt, expectedGateway string) (Identity, error) {
	return v.VerifySupervisionReceiptContext(context.Background(), s, expectedGateway)
}
func (v *Verifier) VerifySupervisionReceiptContext(ctx context.Context, s SignedSupervisionReceipt, expectedGateway string) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if s.Version != Version || expectedGateway == "" || s.Receipt.Challenge.GatewayPrincipal != expectedGateway || !boundedSupervision(s) || !supervisionDigest(s.Receipt.AckSHA256) || !v.supervisionScope(s.Receipt.Challenge, now) || !supervisionGrant(s.Grant.Grant, s.Receipt.Challenge, true) || !verify(s.Grant.Grant.PublicKey, "supervision-receipt", supervisionPayload(s.Version, s.Grant, s.Receipt), s.Signature) {
		return Identity{}, ErrDenied
	}
	return v.identityContext(ctx, s.Grant, "supervision-server", now, now)
}

// SupervisionAckDigest hashes the canonical full signed ACK, including its
// signed grant and signature. VerifySupervisionAckProof returns exactly these
// canonical bytes as Operator.Proof.
func SupervisionAckDigest(s SignedSupervisionAck) string { b, _ := json.Marshal(s); return hash(b) }
