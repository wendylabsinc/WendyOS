package authority

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"
)

// SignedTaskAudit attests what the assignee exported now, not what it signed at
// the historical event time. It is a read-only purpose and cannot admit a task.
type SignedTaskAudit struct {
	Version        int             `json:"version"`
	Grant          SignedGrant     `json:"grant"`
	Audience       string          `json:"audience"`
	Assignee       string          `json:"assignee"`
	RunID          string          `json:"run_id"`
	Incarnation    string          `json:"incarnation"`
	Sequence       uint64          `json:"sequence"`
	PreviousSHA256 string          `json:"previous_sha256"`
	Event          json.RawMessage `json:"event"`
	ExportedUnixMS int64           `json:"exported_unix_ms"`
	Signature      []byte          `json:"signature"`
}

func taskAuditPayload(s SignedTaskAudit) any {
	g, _ := json.Marshal(s.Grant)
	return struct {
		Version        int             `json:"version"`
		GrantDigest    string          `json:"grant_digest"`
		Audience       string          `json:"audience"`
		Assignee       string          `json:"assignee"`
		RunID          string          `json:"run_id"`
		Incarnation    string          `json:"incarnation"`
		Sequence       uint64          `json:"sequence"`
		PreviousSHA256 string          `json:"previous_sha256"`
		Event          json.RawMessage `json:"event"`
		ExportedUnixMS int64           `json:"exported_unix_ms"`
	}{s.Version, hash(g), s.Audience, s.Assignee, s.RunID, s.Incarnation, s.Sequence, s.PreviousSHA256, s.Event, s.ExportedUnixMS}
}
func validTaskAudit(s SignedTaskAudit) bool {
	g := s.Grant.Grant
	return s.Version == Version && s.Sequence > 0 && s.ExportedUnixMS > 0 && len(s.RunID) > 0 && len(s.RunID) <= 256 && supervisionDigest(s.Incarnation) && supervisionDigest(s.PreviousSHA256) && len(s.Event) > 0 && len(s.Event) <= MaxTaskStatusRecordBytes && json.Valid(s.Event) && g.Kind == "device" && contains(g.Scopes, "task-status") && s.Audience == g.TaskStatusAudience && s.Assignee == g.TaskStatusAssignee
}
func SignTaskAudit(s SignedTaskAudit, key ed25519.PrivateKey) (SignedTaskAudit, error) {
	s.Version = Version
	s.Event = append(json.RawMessage(nil), s.Event...)
	if !validTaskAudit(s) || !privateMatches(key, s.Grant.Grant.PublicKey) || validateGrant(s.Grant.Grant, 365*24*time.Hour) != nil || s.ExportedUnixMS < s.Grant.Grant.NotBeforeUnixMS || s.ExportedUnixMS >= s.Grant.Grant.NotAfterUnixMS {
		return SignedTaskAudit{}, ErrDenied
	}
	var err error
	s.Signature, err = sign(key, "task-audit-history-v1", taskAuditPayload(s))
	return s, err
}

// current is mandatory for a new import/export. Retained history can be checked
// without current grant/revocation validity, but never becomes current authority.
func (v *Verifier) VerifyTaskAudit(ctx context.Context, s SignedTaskAudit, current bool) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	g := s.Grant.Grant
	if !validTaskAudit(s) || s.ExportedUnixMS > now.UnixMilli() || !verify(g.PublicKey, "task-audit-history-v1", taskAuditPayload(s), s.Signature) || validateGrant(g, v.cfg.MaxGrantLifetime) != nil || !verify(v.cfg.RootPublicKey, "grant", g, s.Grant.Signature) || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != v.cfg.Domain || s.ExportedUnixMS < g.NotBeforeUnixMS || s.ExportedUnixMS >= g.NotAfterUnixMS {
		return Identity{}, ErrDenied
	}
	if current {
		return v.identityContext(ctx, s.Grant, "task-status", now, now)
	}
	return grantIdentity(g), nil
}
